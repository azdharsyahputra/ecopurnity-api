package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

var errAuctionNotFound = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Auction tidak ditemukan"}

func viewerID(ctx context.Context) string {
	if sess := state(ctx).session; sess != nil && sess.Status != "suspended" {
		return sess.UserID
	}
	return ""
}

func (s *Server) ListAuctions(ctx context.Context, req api.ListAuctionsRequestObject) (api.ListAuctionsResponseObject, error) {
	p := req.Params
	page, size := pageParams(p.Page, p.PageSize)
	var where []string
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	where = append(where, "true")
	if p.Q != nil && strings.TrimSpace(*p.Q) != "" {
		ph := arg("%" + likeEscape(strings.TrimSpace(*p.Q)) + "%")
		where = append(where, fmt.Sprintf("(a.title ILIKE %[1]s OR a.code ILIKE %[1]s OR coalesce(m.name, '') ILIKE %[1]s)", ph))
	}
	if p.Category != nil {
		where = append(where, "a.category_id = "+arg(string(*p.Category)))
	}
	if p.Region != nil && strings.TrimSpace(*p.Region) != "" {
		where = append(where, "m.region = "+arg(strings.TrimSpace(*p.Region)))
	}
	if p.Status != nil && strings.TrimSpace(*p.Status) != "" {
		where = append(where, "a.status = ANY("+arg(strings.Split(*p.Status, ","))+")")
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.DB.Reader().QueryRow(ctx, `SELECT count(*) FROM auctions a LEFT JOIN markets m ON m.id = a.market_id WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, err
	}
	data, err := loadAuctions(ctx, s.DB.Reader(), cond+`
		ORDER BY CASE a.status WHEN 'live' THEN 0 WHEN 'extended' THEN 0 WHEN 'qualification' THEN 1 WHEN 'scheduled' THEN 2 ELSE 3 END, a.ends_at, a.id
		LIMIT `+arg(size)+` OFFSET `+arg((page-1)*size), args...)
	if err != nil {
		return nil, err
	}
	return api.ListAuctions200JSONResponse{Data: data, Meta: api.PageMeta{Page: page, PageSize: size, Total: total}}, nil
}

func (s *Server) GetAuction(ctx context.Context, req api.GetAuctionRequestObject) (api.GetAuctionResponseObject, error) {

	r, err := loadAuction(ctx, s.DB.Primary(), req.Id, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errAuctionNotFound
	}
	if err != nil {
		return nil, err
	}
	d, err := auctionDetail(ctx, s.DB.Primary(), r, viewerID(ctx))
	if err != nil {
		return nil, err
	}
	return api.GetAuction200JSONResponse(d), nil
}

func qualificationOf(ctx context.Context, q dbtx, auctionID, userID string) (api.Qualification, error) {
	var status string
	var emailVerified bool
	err := q.QueryRow(ctx, `
		SELECT coalesce((SELECT status FROM auction_qualifications WHERE auction_id::text = $1 AND user_id = $2), 'not_started'),
		       (SELECT email_verified_at IS NOT NULL FROM users WHERE id = $2)`, auctionID, userID).Scan(&status, &emailVerified)
	if err != nil {
		return api.Qualification{}, err
	}
	done := status == "qualified"
	check := func(id, label string, ok bool, detail string) struct {
		Detail *string `json:"detail,omitempty"`
		Done   bool    `json:"done"`
		Id     string  `json:"id"`
		Label  string  `json:"label"`
	} {
		c := struct {
			Detail *string `json:"detail,omitempty"`
			Done   bool    `json:"done"`
			Id     string  `json:"id"`
			Label  string  `json:"label"`
		}{Id: id, Label: label, Done: ok}
		if detail != "" {
			c.Detail = &detail
		}
		return c
	}
	score, trades, err := userReputation(ctx, q, userID)
	if err != nil {
		return api.Qualification{}, err
	}
	repOK, repDetail := reputationGate(score, trades)
	emailDetail := ""
	if !emailVerified {
		emailDetail = "Verifikasi dari halaman profil"
	}
	qual := api.Qualification{AuctionId: auctionID, Status: api.QualificationStatus(status)}
	qual.Checks = append(qual.Checks,
		check("email", "Email terverifikasi", emailVerified, emailDetail),
		check("reputation", "Reputasi ≥ 80", repOK, repDetail),
		check("document", "Dokumen spesifikasi", done, ""),
		check("rules", "Menyetujui aturan auction", done, ""),
	)
	return qual, nil
}

func reputationGate(score, trades int) (bool, string) {
	switch {
	case trades == 0:
		return true, "Akun baru: diizinkan untuk lot pertama"
	case score < baselineScore:
		return false, fmt.Sprintf("Skor reputasimu %d, minimal %d", score, baselineScore)
	}
	return true, fmt.Sprintf("Skor %d", score)
}

func (s *Server) QualifyForAuction(ctx context.Context, req api.QualifyForAuctionRequestObject) (api.QualifyForAuctionResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	var out api.Qualification
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var auctionID string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM auctions WHERE id::text = $1`, req.Id).Scan(&auctionID); errors.Is(err, pgx.ErrNoRows) {
			return errAuctionNotFound
		} else if err != nil {
			return err
		}
		var emailVerified bool
		if err := tx.QueryRow(ctx, `SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1`, sess.UserID).Scan(&emailVerified); err != nil {
			return err
		}
		f := map[string]string{}
		if strings.TrimSpace(req.Body.DocumentName) == "" {
			f["document"] = "Unggah dokumen spesifikasi"
		}
		if !req.Body.AcceptRules {
			f["rules"] = "Setujui aturan auction"
		}
		if !emailVerified {
			f["email"] = "Verifikasi email dulu"
		}
		score, trades, err := userReputation(ctx, tx, sess.UserID)
		if err != nil {
			return err
		}
		if ok, detail := reputationGate(score, trades); !ok {
			f["reputation"] = detail
		}
		if len(f) > 0 {
			return &Error{Status: 422, Code: "validation", Message: "Syarat kualifikasi belum lengkap", Fields: f}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO auction_qualifications (auction_id, user_id, status, document_name, rules_accepted_at) VALUES ($1, $2, 'qualified', $3, now())
			ON CONFLICT (auction_id, user_id) DO UPDATE SET status = 'qualified', document_name = EXCLUDED.document_name, rules_accepted_at = now()`,
			auctionID, sess.UserID, strings.TrimSpace(req.Body.DocumentName)); err != nil {
			return err
		}
		out, err = qualificationOf(ctx, tx, auctionID, sess.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.QualifyForAuction200JSONResponse(out), nil
}

func isOwner(ctx context.Context, q dbtx, r auctionRow, userID string) (bool, error) {
	if r.OwnerUserID != nil && *r.OwnerUserID == userID {
		return true, nil
	}
	if r.OwnerOrgID == nil {
		return false, nil
	}
	var member bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_members WHERE org_id::text = $1 AND user_id = $2 AND status = 'active')`, *r.OwnerOrgID, userID).Scan(&member)
	return member, err
}

func (s *Server) GetMyAuctionState(ctx context.Context, req api.GetMyAuctionStateRequestObject) (api.GetMyAuctionStateResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Primary()
	r, err := loadAuction(ctx, q, req.Id, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errAuctionNotFound
	}
	if err != nil {
		return nil, err
	}
	qual, err := qualificationOf(ctx, q, r.ID, sess.UserID)
	if err != nil {
		return nil, err
	}
	bid, err := myBid(ctx, q, r, sess.UserID)
	if err != nil {
		return nil, err
	}
	out := myAuctionState{Qualification: qual, Bid: bid}
	switch {
	case r.OwnerUserID != nil && *r.OwnerUserID == sess.UserID:
		out.Owner, out.EvaluateHref = true, ptr("/app/auctions/"+r.ID+"/evaluate")
	case r.OwnerOrgID != nil:

		var view bool
		if err := q.QueryRow(ctx, `
			SELECT m.role = 'owner' OR 'auctions.view' = ANY(ro.permissions)
			FROM org_members m JOIN org_roles ro ON ro.org_id = m.org_id AND ro.key = m.role
			WHERE m.org_id::text = $1 AND m.user_id = $2 AND m.status = 'active'`, *r.OwnerOrgID, sess.UserID).Scan(&view); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if view {

			out.Owner, out.EvaluateHref = true, ptr("/org/"+*r.OwnerOrgID+"/auctions/"+deref(r.OrgAuctionID)+"/evaluate")
		}
	}
	return out, nil
}

type myAuctionState struct {
	Qualification api.Qualification `json:"qualification"`
	Bid           *api.MyBid        `json:"bid"`
	Owner         bool              `json:"owner"`
	EvaluateHref  *string           `json:"evaluateHref,omitempty"`
}

func (m myAuctionState) VisitGetMyAuctionStateResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(m)
}

func myBid(ctx context.Context, q dbtx, r auctionRow, userID string) (*api.MyBid, error) {
	var id, status string
	var price int64
	var capacity *float64
	var submitted, updated time.Time
	err := q.QueryRow(ctx, `
		SELECT id, price_idr, status, (SELECT min(created_at) FROM bids f WHERE f.auction_id = b.auction_id AND f.bidder_user_id = b.bidder_user_id), updated_at, capacity::float8
		FROM bids b WHERE auction_id = $1 AND bidder_user_id = $2
		ORDER BY (status = 'withdrawn'), seq DESC LIMIT 1`, r.ID, userID).Scan(&id, &price, &status, &submitted, &updated, &capacity)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	live := r.Status == "live" || r.Status == "extended"
	b := &api.MyBid{Auction: r.api(), PriceIdr: int(price), Status: api.BidStatus(status), SubmittedAt: submitted, UpdatedAt: updated,
		CanWithdraw: withdrawBlock(r, status, time.Now()) == ""}
	if r.Type == "reverse" || r.Type == "sealed" {
		b.Capacity = &api.Quantity{Value: r.Quantity, Unit: r.Unit}
		if capacity != nil {
			b.Capacity.Value = *capacity
		}
	}
	if !(r.Type == "sealed" && live) && status != "withdrawn" {
		rank, err := rankOf(ctx, q, r, userID, price)
		if err != nil {
			return nil, err
		}
		b.Rank = &rank
	}
	return b, nil
}

func withdrawBlock(r auctionRow, status string, now time.Time) string {
	rule := "before_last_30"
	if r.WithdrawRule != nil {
		rule = *r.WithdrawRule
	}
	switch {
	case r.Status != "live" && r.Status != "extended":
		return "Auction tidak sedang berjalan"
	case status == "withdrawn":
		return "Bid sudah ditarik"
	case rule == "never":
		return "Bid di auction ini mengikat, tidak bisa ditarik"
	case rule == "anytime" && status == "leading":
		return "Bid terdepan tidak bisa ditarik"
	case rule == "before_last_30" && (status == "leading" || r.EndsAt.Sub(now) <= 30*time.Minute):
		return "Bid terdepan atau 30 menit terakhir tidak bisa ditarik"
	}
	return ""
}

func rankOf(ctx context.Context, q dbtx, r auctionRow, userID string, price int64) (int, error) {
	cmp := ">="
	agg := "max"
	if r.Type != "forward" {
		cmp, agg = "<=", "min"
	}
	var n int
	err := q.QueryRow(ctx, fmt.Sprintf(`
		SELECT count(*) FROM (
		  SELECT bidder_party_id, %s(price_idr) AS best FROM bids
		  WHERE auction_id = $1 AND status <> 'withdrawn' AND bidder_user_id IS DISTINCT FROM $2
		  GROUP BY bidder_party_id) o
		WHERE o.best %s $3`, agg, cmp), r.ID, userID, price).Scan(&n)
	return n + 1, err
}

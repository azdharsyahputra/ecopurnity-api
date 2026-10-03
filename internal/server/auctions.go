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

// Auctions: public reads, qualification, the caller's state. Bidding and awards are in bids.go, the clock in
// auction_engine.go.

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
	// Primary: right after placing a bid the room refetches and must see it.
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

// ── Qualification ────────────────────────────────────────────────

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
	emailDetail := ""
	if !emailVerified {
		emailDetail = "Verifikasi dari halaman profil"
	}
	qual := api.Qualification{AuctionId: auctionID, Status: api.QualificationStatus(status)}
	qual.Checks = append(qual.Checks,
		check("email", "Email terverifikasi", emailVerified, emailDetail),
		// ponytail: reputation gate passes until the reputation area computes scores (new accounts start at the 80 baseline).
		check("reputation", "Reputasi ≥ 80", true, "Akun baru: diizinkan untuk lot pertama"),
		check("document", "Dokumen spesifikasi", done, ""),
		check("rules", "Menyetujui aturan auction", done, ""),
	)
	return qual, nil
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

// isOwner: the caller created the auction as a buyer, or belongs to the org that owns it.
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
	owner, err := isOwner(ctx, q, r, sess.UserID)
	if err != nil {
		return nil, err
	}
	out := myAuctionState{Qualification: qual, Bid: bid, Owner: owner}
	if owner {
		href := "/app/auctions/" + r.ID + "/evaluate"
		if r.OwnerOrgID != nil {
			href = "/org/" + *r.OwnerOrgID + "/auctions/" + r.ID + "/evaluate"
		}
		out.EvaluateHref = &href
	}
	return out, nil
}

// myAuctionState writes PersonalAuctionMe with `bid: null` when the caller has not bid (the generated type can't
// express a nullable union member).
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

// myBid is the caller's current bid (latest non-withdrawn, or the withdrawn one if that is all there is) with rank.
func myBid(ctx context.Context, q dbtx, r auctionRow, userID string) (*api.MyBid, error) {
	var id, status string
	var price int64
	var submitted, updated time.Time
	err := q.QueryRow(ctx, `
		SELECT id, price_idr, status, (SELECT min(created_at) FROM bids f WHERE f.auction_id = b.auction_id AND f.bidder_user_id = b.bidder_user_id), updated_at
		FROM bids b WHERE auction_id = $1 AND bidder_user_id = $2
		ORDER BY (status = 'withdrawn'), seq DESC LIMIT 1`, r.ID, userID).Scan(&id, &price, &status, &submitted, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	live := r.Status == "live" || r.Status == "extended"
	b := &api.MyBid{Auction: r.api(), PriceIdr: int(price), Status: api.BidStatus(status), SubmittedAt: submitted, UpdatedAt: updated,
		CanWithdraw: live && status != "leading" && status != "withdrawn" && time.Until(r.EndsAt) > 30*time.Minute}
	if !(r.Type == "sealed" && live) && status != "withdrawn" {
		rank, err := rankOf(ctx, q, r, userID, price)
		if err != nil {
			return nil, err
		}
		b.Rank = &rank
	}
	return b, nil
}

// rankOf: 1 + the number of other bidders whose best active price is at least as good (ties favour the earlier bid).
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

package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

type dashAction = struct {
	Detail string                          `json:"detail"`
	Href   string                          `json:"href"`
	Id     string                          `json:"id"`
	Title  string                          `json:"title"`
	Tone   api.DashboardSummaryActionsTone `json:"tone"`
}

func (s *Server) GetMyDashboard(ctx context.Context, _ api.GetMyDashboardRequestObject) (api.GetMyDashboardResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Primary()
	var d api.DashboardSummary
	d.Actions = []dashAction{}
	parties, err := userPartyIDs(ctx, q, sess.UserID)
	if err != nil {
		return nil, err
	}

	if err := q.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM listings WHERE owner_party_id = ANY($1::uuid[]) AND kind = 'demand' AND status IN ('open','matched','in_market')),
		  (SELECT count(*) FROM listings WHERE owner_party_id = ANY($1::uuid[]) AND kind = 'supply' AND status IN ('available','in_market')),
		  (SELECT count(*) FROM trades WHERE (buyer_party_id = ANY($1::uuid[]) OR supplier_party_id = ANY($1::uuid[]))
		     AND status NOT IN ('completed','cancelled')),
		  (SELECT coalesce(sum(total_idr), 0) FROM trades WHERE supplier_party_id = ANY($1::uuid[]) AND status = 'completed'
		     AND updated_at > now() - interval '30 days'),
		  -- Savings: what buyer-side reverse/sealed auction wins saved against the opening price.
		  (SELECT coalesce(sum(greatest(0, a.opening_price_idr - t.unit_price_idr) * t.quantity), 0)::bigint
		     FROM trades t JOIN auctions a ON a.id = t.auction_id
		     WHERE t.buyer_party_id = ANY($1::uuid[]) AND a.type IN ('reverse','sealed') AND t.status <> 'cancelled'
		       AND t.created_at > now() - interval '30 days')`, parties).
		Scan(&d.Stats.OpenDemands, &d.Stats.CurrentOffers, &d.Stats.RunningTransactions, &d.Stats.Earnings30dIdr, &d.Stats.Savings30dIdr); err != nil {
		return nil, err
	}
	rep, err := partyReputation(ctx, q, parties)
	if err != nil {
		return nil, err
	}
	d.Stats.Reputation = float64(rep.Score)

	d.LiveBids = []api.MyBid{}
	rows, err := q.Query(ctx, auctionSelect+`
		WHERE a.status IN ('live','extended')
		  AND EXISTS (SELECT 1 FROM bids b WHERE b.auction_id = a.id AND b.bidder_user_id = $1 AND b.status <> 'withdrawn')
		ORDER BY a.ends_at, a.id`, sess.UserID)
	if err != nil {
		return nil, err
	}
	var live []auctionRow
	for rows.Next() {
		r, err := scanAuction(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		live = append(live, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var ending []dashAction
	for _, r := range live {
		b, err := myBid(ctx, q, r, sess.UserID)
		if err != nil {
			return nil, err
		}
		if b == nil || b.Status == api.BidStatusWithdrawn {
			continue
		}
		d.LiveBids = append(d.LiveBids, *b)
		if b.Status == api.BidStatusOutbid {
			rank := "–"
			if b.Rank != nil {
				rank = fmt.Sprint(*b.Rank)
			}
			d.Actions = append(d.Actions, dashAction{Id: "ob-" + r.ID, Tone: "red", Title: "Tersalip di " + r.Title,
				Detail: fmt.Sprintf("Bid kamu %s · peringkat %s", rupiah(int64(b.PriceIdr)), rank), Href: "/auctions/" + r.ID})
		}
		if time.Until(r.EndsAt) < time.Hour {
			ending = append(ending, dashAction{Id: "end-" + r.ID, Tone: "orange", Title: r.Title + " berakhir < 1 jam",
				Detail: "Pastikan bid terakhirmu sudah masuk", Href: "/auctions/" + r.ID})
		}
	}
	d.Stats.ActiveBids = len(d.LiveBids)
	d.Actions = append(d.Actions, ending...)

	rows, err = q.Query(ctx, `
		SELECT t.id::text, t.title, t.status, t.terms, CASE WHEN t.buyer_party_id = ANY($1::uuid[]) THEN 'buyer' ELSE 'supplier' END AS role,
		       EXISTS (SELECT 1 FROM trade_acceptances x WHERE x.trade_id = t.id AND x.side = 'buyer'),
		       EXISTS (SELECT 1 FROM trade_acceptances x WHERE x.trade_id = t.id AND x.side = 'supplier'),
		       (t.quantity - coalesce((SELECT sum(sh.quantity) FROM shipments sh WHERE sh.trade_id = t.id), 0))::float8,
		       (SELECT count(*) FROM shipments sh WHERE sh.trade_id = t.id AND sh.status <> 'delivered'),
		       EXISTS (SELECT 1 FROM reviews r WHERE r.trade_id = t.id
		               AND r.side = CASE WHEN t.buyer_party_id = ANY($1::uuid[]) THEN 'buyer' ELSE 'supplier' END)
		FROM trades t WHERE t.buyer_party_id = ANY($1::uuid[]) OR t.supplier_party_id = ANY($1::uuid[])
		ORDER BY t.updated_at DESC LIMIT 200`, parties)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, title, side string
		var accB, accS, reviewed bool
		t := tradeState{Agreement: map[string]bool{}, Reviewed: map[string]bool{}}
		if err := rows.Scan(&id, &title, &t.Status, &t.Terms, &side, &accB, &accS, &t.UnscheduledQty, &t.OpenShipments, &reviewed); err != nil {
			rows.Close()
			return nil, err
		}
		t.Agreement["buyer"], t.Agreement["supplier"], t.Reviewed[side] = accB, accS, reviewed
		n := 0
		for _, a := range tradeActions(t, side) {
			if a != "cancel" && a != "dispute" && a != "review" {
				n++
			}
		}
		if n > 0 {
			d.Actions = append(d.Actions, dashAction{Id: "tx-" + id, Tone: "blue", Title: title,
				Detail: fmt.Sprintf("Menunggu kamu: %d aksi", n), Href: "/app/transactions/" + id})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	opps, err := personalOpportunities(ctx, q, sess.UserID)
	if err != nil {
		return nil, err
	}
	d.TopMatches = []api.PersonalOpportunity{}
	lime := false
	for _, o := range opps {
		if o.Relation != api.PersonalOpportunityRelationNone {
			d.Stats.ActiveOpportunities++
			continue
		}
		if len(d.TopMatches) < 3 {
			d.TopMatches = append(d.TopMatches, o)
		}
		if !lime && o.Status == api.OpportunityStatusDetected && len(o.Reasons) >= 2 {
			lime = true
			labels := make([]string, len(o.Reasons))
			for i, r := range o.Reasons {
				labels[i] = r.Label
			}
			d.Actions = append(d.Actions, dashAction{Id: "opp-" + o.Id, Tone: "lime", Title: "Match baru: " + o.Title,
				Detail: strings.Join(labels, " · "), Href: "/opportunities/" + o.Id})
		}
	}

	d.Activity = s.publicActivity(ctx, 6)

	d.Connections.Sample = []api.PartyRef{}
	rows, err = q.Query(ctx, `
		SELECT p.name, p.display_kind, p.verified, count(*) OVER () FROM (
		  SELECT cp, max(created_at) AS at FROM (
		    SELECT CASE WHEN buyer_party_id = ANY($1::uuid[]) THEN supplier_party_id ELSE buyer_party_id END AS cp, created_at
		    FROM trades WHERE buyer_party_id = ANY($1::uuid[]) OR supplier_party_id = ANY($1::uuid[])) t
		  GROUP BY cp) c JOIN parties p ON p.id = c.cp
		ORDER BY c.at DESC LIMIT 3`, parties)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p api.PartyRef
		if err := rows.Scan(&p.Name, &p.Kind, &p.Verified, &d.Connections.Count); err != nil {
			rows.Close()
			return nil, err
		}
		d.Connections.Sample = append(d.Connections.Sample, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return api.GetMyDashboard200JSONResponse(d), nil
}

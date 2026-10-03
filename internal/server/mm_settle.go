package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Aggregated settlement of market rounds (PRD F6, mocks/settle.ts): the winning offer of a closed round is split
// pro-rata across the members who contributed to the lot (demand in procurement rounds, supply in selling rounds), one
// trade per member. A collective pool round splits over the pool's member businesses instead, each getting its own
// sub-PO. The auction clock already marked the bids won/lost at close; settlement creates the trades, once.

var errRoundNotFound = &Error{Status: http.StatusNotFound, Code: "not_found", Message: "Round tidak ditemukan"}

// settlementRound: the round of an operated market. Anything else — signed out, no market_maker capability, a market
// the caller does not operate, a round of another market — is the same 404 (spec).
func (s *Server) settlementRound(ctx context.Context, q dbtx, marketID, auctionID string, lock bool) (*session, mmMarket, auctionRow, error) {
	sess, err := s.requireMaker(ctx)
	if err != nil {
		var apiErr *Error
		if errors.As(err, &apiErr) {
			err = errRoundNotFound
		}
		return nil, mmMarket{}, auctionRow{}, err
	}
	m, err := operatedMarket(ctx, q, sess.UserID, marketID, false)
	if errors.Is(err, errMarketNotFound) {
		err = errRoundNotFound
	}
	if err != nil {
		return nil, m, auctionRow{}, err
	}
	r, err := loadAuction(ctx, q, auctionID, lock)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (r.MarketID == nil || *r.MarketID != m.ID)) {
		err = errRoundNotFound
	}
	return sess, m, r, err
}

// settleLine is one member's share plus what settling it needs.
type settleLine struct {
	MemberID, Member, Location string
	UserID, OrgID, PartyID     *string // PartyID nil: nothing is stored for this member (external pool business)
	PoolMemberID               string
	Quantity, Share            float64
	AmountIdr                  int64
}

type settlementPreview struct {
	Side, Winner      string
	WinnerUserID      *string
	WinnerParty       string // "" when nobody bid
	PriceIdr          int64
	Lines             []settleLine
	PoolID, PoolTitle string
	SettledAt         *time.Time
	SettledBy         string
	SettledTrades     int
}

func previewSettlement(ctx context.Context, q dbtx, r auctionRow) (settlementPreview, error) {
	pv := settlementPreview{Side: "selling"}
	if r.lowerWins() {
		pv.Side = "procurement"
	}
	os, err := offers(ctx, q, r)
	if err != nil {
		return pv, err
	}
	switch {
	case len(os) > 0:
		pv.PriceIdr, pv.Winner, pv.WinnerUserID, pv.WinnerParty = os[0].Price, os[0].Name, os[0].UserID, os[0].PartyID
	case r.Current != nil:
		pv.PriceIdr = *r.Current
	default:
		pv.PriceIdr = r.Opening
	}
	if pv.Winner == "" {
		pv.Winner = map[string]string{"procurement": "Supplier terpilih", "selling": "Pembeli terpilih"}[pv.Side]
	}
	if err := q.QueryRow(ctx, `
		SELECT s.settled_at, u.name, (SELECT count(*) FROM settlement_lines sl WHERE sl.settlement_id = s.id AND sl.trade_id IS NOT NULL)
		FROM settlements s JOIN users u ON u.id = s.settled_by WHERE s.auction_id = $1`, r.ID).Scan(&pv.SettledAt, &pv.SettledBy, &pv.SettledTrades); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return pv, err
	}
	err = q.QueryRow(ctx, `SELECT id::text, title FROM collective_pools WHERE auction_id = $1`, r.ID).Scan(&pv.PoolID, &pv.PoolTitle)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return pv, err
	}
	var members []member
	if pv.PoolID != "" {
		// Collective pool round: the lot goes back to the pool's member businesses (names masked unless they opted in).
		rows, err := q.Query(ctx, `
			SELECT pm.id::text, pm.org_id::text, pm.opt_in, coalesce(o.name, pm.name), pm.quantity, p.id::text,
			       coalesce(nullif(btrim(pm.drop_point), ''),
			                (SELECT nullif(btrim(pr.delivery_location), '') FROM procurement_requests pr
			                 WHERE pr.pool_id = pm.pool_id AND pr.org_id = pm.org_id AND pr.status = 'in_collective' ORDER BY pr.created_at LIMIT 1),
			                (SELECT w.name || ', ' || w.location FROM org_warehouses w WHERE w.org_id = pm.org_id ORDER BY w.created_at, w.id LIMIT 1),
			                nullif(btrim(op.location), ''), '-')
			FROM pool_members pm LEFT JOIN orgs o ON o.id = pm.org_id LEFT JOIN parties p ON p.org_id = pm.org_id
			LEFT JOIN org_profiles op ON op.org_id = pm.org_id
			WHERE pm.pool_id = $1 ORDER BY pm.created_at, pm.id`, pv.PoolID)
		if err != nil {
			return pv, err
		}
		for i := 0; rows.Next(); i++ {
			var l settleLine
			var optIn bool
			var qty float64
			if err := rows.Scan(&l.PoolMemberID, &l.OrgID, &optIn, &l.Member, &qty, &l.PartyID, &l.Location); err != nil {
				rows.Close()
				return pv, err
			}
			l.MemberID = fmt.Sprintf("pool:%d", i)
			if !optIn {
				l.Member = fmt.Sprintf("Bisnis lain #%d", i+1)
			}
			pv.Lines = append(pv.Lines, l)
			members = append(members, member{ID: l.MemberID, Quantity: qty})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return pv, err
		}
	} else {
		// Platform accounts' in-market listings on the round's side, then up to four active participants that are not
		// platform accounts filling the rest of the lot.
		kind, role := "supply", "supplier"
		if pv.Side == "procurement" {
			kind, role = "demand", "buyer"
		}
		rows, err := q.Query(ctx, `
			SELECT 'l:' || l.id, p.name, l.location, p.user_id::text, p.org_id::text, p.id::text, l.quantity
			FROM listings l JOIN parties p ON p.id = l.owner_party_id
			WHERE l.market_id = $1 AND l.kind = $2 AND l.status = 'in_market' AND p.kind <> 'external'
			ORDER BY l.created_at, l.id`, *r.MarketID, kind)
		if err != nil {
			return pv, err
		}
		var contrib []member
		for rows.Next() {
			var l settleLine
			var qty float64
			if err := rows.Scan(&l.MemberID, &l.Member, &l.Location, &l.UserID, &l.OrgID, &l.PartyID, &qty); err != nil {
				rows.Close()
				return pv, err
			}
			pv.Lines = append(pv.Lines, l)
			contrib = append(contrib, member{ID: l.MemberID, Quantity: qty})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return pv, err
		}
		frows, err := q.Query(ctx, `
			SELECT 'p:' || mp.id, p.name, p.org_id::text, p.id::text
			FROM market_participants mp JOIN parties p ON p.id = mp.party_id
			WHERE mp.market_id = $1 AND mp.status = 'active' AND mp.role = $2 AND p.user_id IS NULL
			ORDER BY mp.joined_at, mp.id LIMIT 4`, *r.MarketID, role)
		if err != nil {
			return pv, err
		}
		var fillers []string
		for frows.Next() {
			l := settleLine{Location: "-"}
			if err := frows.Scan(&l.MemberID, &l.Member, &l.OrgID, &l.PartyID); err != nil {
				frows.Close()
				return pv, err
			}
			pv.Lines = append(pv.Lines, l)
			fillers = append(fillers, l.MemberID)
		}
		frows.Close()
		if err := frows.Err(); err != nil {
			return pv, err
		}
		members = membersForLot(r.Quantity, contrib, fillers)
		pv.Lines = pv.Lines[:len(members)] // fillers get nothing when the contributions cover the lot
	}
	split := splitProRata(r.Quantity, members)
	lines := pv.Lines[:0]
	for i, l := range pv.Lines {
		if split[i].Quantity <= 0 {
			continue
		}
		l.Quantity, l.Share, l.AmountIdr = split[i].Quantity, split[i].Share, int64(split[i].Quantity)*pv.PriceIdr
		lines = append(lines, l)
	}
	pv.Lines = lines
	return pv, nil
}

func (pv settlementPreview) api(r auctionRow) api.MmSettlementResult {
	out := api.MmSettlementResult{AuctionId: r.ID, Title: r.Title, Side: api.MmSettlementResultSide(pv.Side), Unit: r.Unit, LotQty: r.Quantity,
		PriceIdr: int(pv.PriceIdr), Winner: pv.Winner, WinnerUserId: pv.WinnerUserID}
	out.Lines = []struct {
		AmountIdr int     `json:"amountIdr"`
		Member    string  `json:"member"`
		MemberId  string  `json:"memberId"`
		OrgId     *string `json:"orgId,omitempty"`
		Quantity  float64 `json:"quantity"`
		Share     float64 `json:"share"`
		UserId    *string `json:"userId,omitempty"`
	}{}
	for _, l := range pv.Lines {
		line := struct {
			AmountIdr int     `json:"amountIdr"`
			Member    string  `json:"member"`
			MemberId  string  `json:"memberId"`
			OrgId     *string `json:"orgId,omitempty"`
			Quantity  float64 `json:"quantity"`
			Share     float64 `json:"share"`
			UserId    *string `json:"userId,omitempty"`
		}{AmountIdr: int(l.AmountIdr), Member: l.Member, MemberId: l.MemberID, Quantity: l.Quantity, Share: l.Share, UserId: l.UserID}
		if pv.PoolID != "" {
			line.OrgId = l.OrgID // marks a business pool member (sub-PO)
		}
		out.Lines = append(out.Lines, line)
	}
	if pv.SettledAt != nil {
		out.Settled = &struct {
			At     time.Time `json:"at"`
			By     string    `json:"by"`
			Trades int       `json:"trades"`
		}{*pv.SettledAt, pv.SettledBy, pv.SettledTrades}
	}
	return out
}

func (s *Server) GetMmRoundSettlement(ctx context.Context, req api.GetMmRoundSettlementRequestObject) (api.GetMmRoundSettlementResponseObject, error) {
	q := s.DB.Primary()
	_, _, r, err := s.settlementRound(ctx, q, req.Id, req.Aid, false)
	if err != nil {
		return nil, err
	}
	pv, err := previewSettlement(ctx, q, r)
	if err != nil {
		return nil, err
	}
	return api.GetMmRoundSettlement200JSONResponse(pv.api(r)), nil
}

func (s *Server) SettleMmRound(ctx context.Context, req api.SettleMmRoundRequestObject) (api.SettleMmRoundResponseObject, error) {
	var out api.MmSettlementResult
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		sess, m, r, err := s.settlementRound(ctx, tx, req.Id, req.Aid, true)
		if err != nil {
			return err
		}
		if r.Status != "closed" && r.Status != "awarded" {
			return &Error{Status: http.StatusConflict, Code: "not_closed", Message: "Settlement hanya untuk round yang sudah ditutup"}
		}
		// A recorded settlement, or a Dutch round whose accept already made the trade.
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM settlements WHERE auction_id = $1) OR EXISTS (SELECT 1 FROM trades WHERE auction_id = $1)`,
			r.ID).Scan(&done); err != nil {
			return err
		}
		if done {
			return &Error{Status: http.StatusConflict, Code: "already_settled", Message: "Round ini sudah di-settle"}
		}
		pv, err := previewSettlement(ctx, tx, r)
		if err != nil {
			return err
		}
		if pv.WinnerParty == "" {
			return &Error{Status: http.StatusConflict, Code: "no_bids", Message: "Round ini belum punya penawaran untuk di-settle"}
		}
		var settlementID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO settlements (auction_id, side, winner_party_id, price_idr, settled_by) VALUES ($1, $2, $3, $4, $5)
			RETURNING id, settled_at`, r.ID, pv.Side, pv.WinnerParty, pv.PriceIdr, sess.UserID).Scan(&settlementID, new(time.Time)); err != nil {
			return err
		}
		trades := 0
		var changes []change
		for _, l := range pv.Lines {
			changes = append(changes, change{Field: l.Member, After: idNumber(l.Quantity) + " " + r.Unit})
			if l.PartyID == nil {
				continue // external pool business: only in the split
			}
			var tradeID *string
			// A member who is also the winner gets no trade with itself.
			if *l.PartyID != pv.WinnerParty && (pv.PoolID == "" || l.OrgID != nil) {
				id, err := s.settleTrade(ctx, tx, sess, m, r, pv, l)
				if err != nil {
					return err
				}
				tradeID, trades = &id, trades+1
			}
			var lineID string
			if err := tx.QueryRow(ctx, `
				INSERT INTO settlement_lines (settlement_id, member_party_id, quantity, share, amount_idr, trade_id) VALUES ($1, $2, $3, $4, $5, $6)
				RETURNING id`, settlementID, *l.PartyID, l.Quantity, l.Share, l.AmountIdr, tradeID).Scan(&lineID); err != nil {
				return err
			}
			if l.PoolMemberID != "" {
				if _, err := tx.Exec(ctx, `UPDATE pool_members SET settlement_line_id = $2 WHERE id = $1`, l.PoolMemberID, lineID); err != nil {
					return err
				}
			}
		}
		if pv.PoolID != "" {
			if _, err := tx.Exec(ctx, `UPDATE collective_pools SET status = 'settled', settlement_id = $2 WHERE id = $1`, pv.PoolID, settlementID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE auctions SET status = 'awarded' WHERE id = $1`, r.ID); err != nil {
			return err
		}
		if err := emitAuctionState(ctx, tx, r.ID); err != nil {
			return err
		}
		if err := mmAudit(ctx, tx, sess, audit{Action: "Settlement kolektif " + r.Code, EntityType: "auction", EntityID: r.ID, EntityLabel: r.Title,
			MarketID: &m.ID, Changes: changes}, "", nil); err != nil {
			return err
		}
		if err := emitRoundResult(ctx, tx, r.ID); err != nil {
			return err
		}
		if pv, err = previewSettlement(ctx, tx, r); err != nil {
			return err
		}
		out = pv.api(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return api.SettleMmRound200JSONResponse(out), nil
}

// settleTrade creates one member's escrow trade with the winner (maker fee 0.5%) and tells the member about it. Pool
// members get a buyer-side sub-PO delivered to their own drop point; their procurement request moves to po_issued.
func (s *Server) settleTrade(ctx context.Context, tx pgx.Tx, sess *session, m mmMarket, r auctionRow, pv settlementPreview, l settleLine) (string, error) {
	buyer, supplier := *l.PartyID, pv.WinnerParty
	if pv.Side == "selling" {
		buyer, supplier = supplier, buyer
	}
	qty := idNumber(l.Quantity)
	pct := int(math.Round(l.Share * 100))
	title, group := fmt.Sprintf("%s · %s %s (%s)", r.Item, qty, r.Unit, r.Code), "Kolektif "+r.Code
	if pv.PoolID != "" {
		title, group = fmt.Sprintf("%s · %s %s (%s)", pv.PoolTitle, qty, r.Unit, r.Code), "Pool kolektif "+r.Code
	}
	t, err := createTrade(ctx, tx, newTrade{Title: title, BuyerParty: buyer, SupplierParty: supplier, Quantity: l.Quantity, Unit: r.Unit,
		UnitPriceIdr: pv.PriceIdr, MakerFeeRate: 0.005, MarketID: &m.ID, AuctionID: &r.ID, DeliveryAddress: l.Location, Via: "settlement",
		Category: r.Category, Region: m.Region, ActorUserID: &sess.UserID})
	if err != nil {
		return "", err
	}
	// ponytail: the group columns are set here rather than through newTrade, which other areas share.
	if _, err := tx.Exec(ctx, `UPDATE trades SET group_label = $2, group_share = $3 WHERE id = $1`, t.ID, group, l.Share); err != nil {
		return "", err
	}
	if pv.PoolID == "" {
		if l.UserID == nil {
			return t.ID, nil
		}
		return t.ID, notify(ctx, tx, *l.UserID, notification{Type: "transaction_update", Title: "Bagianmu dari " + r.Title,
			Body: fmt.Sprintf("%s %s × %s (%d%% lot)", qty, r.Unit, rupiah(pv.PriceIdr), pct), Href: "/app/transactions/" + t.ID})
	}
	org := *l.OrgID
	if _, err := tx.Exec(ctx, `UPDATE procurement_requests SET status = 'po_issued' WHERE pool_id = $1 AND org_id = $2 AND status = 'in_collective'`,
		pv.PoolID, org); err != nil {
		return "", err
	}
	if err := writeAudit(ctx, tx, audit{ActorUserID: &sess.UserID, ActorLabel: mmActor(sess), Action: "Sub-PO dari pool kolektif " + r.Code,
		EntityType: "transaction", EntityID: t.ID, EntityLabel: t.Code + " " + title, OrgID: &org,
		Changes: []change{{Field: "Bagian pool", After: fmt.Sprintf("%s %s (%d%%) × %s", qty, r.Unit, pct, rupiah(pv.PriceIdr))}}}); err != nil {
		return "", err
	}
	return t.ID, notifyOrg(ctx, tx, org, notification{Type: "transaction_update", Title: "Sub-PO pool: " + pv.PoolTitle,
		Body: fmt.Sprintf("%s %s × %s dari %s. Setujui agreement-nya.", qty, r.Unit, rupiah(pv.PriceIdr), pv.Winner),
		Href: fmt.Sprintf("/org/%s/transactions/%s", org, t.ID)})
}

package server

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// The trade clock: places due standing-contract orders and, when SIMULATE_COUNTERPARTIES is on, plays external
// counterparties. Runs on every API instance; rows are claimed FOR UPDATE SKIP LOCKED, so instances never double-process.
//
// Simulation (demo only, off in production): an external party (parties.kind = 'external', no account) cannot act,
// so a trade or contract with one would stall. The bot takes the external side's next step the way the frontend mock
// does (src/mocks/trade.ts botStep, src/mocks/contracts.ts contractTick): after the trade/proposal has been idle for
// `botDelay`, it accepts the agreement, issues the invoice, pays, ships everything, confirms delivery, accepts QC and
// reviews (5 stars); it accepts contract proposals made to an external party. It never cancels or disputes. Without the
// flag, trades with an external party wait for an operator and proposals to one stay `proposed`.

const botDelay = 8 * time.Second // let the user see each step (mock: 8 s)

var botPriority = []string{"accept_agreement", "issue_invoice", "pay", "ship", "upload_proof", "confirm_receipt", "review"}

// RunTradeClock ticks until ctx ends.
func (s *Server) RunTradeClock(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.ContractTick(ctx, botDelay); err != nil && ctx.Err() == nil && s.Log != nil {
			s.Log.Error("contract clock", "err", err)
		}
		if s.SimulateCounterparties {
			if err := s.TradeCounterpartyTick(ctx, botDelay); err != nil && ctx.Err() == nil && s.Log != nil {
				s.Log.Error("counterparty bot", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// drain runs step until it reports nothing left (at most 100 per tick), one row per transaction.
func (s *Server) drain(ctx context.Context, step func(pgx.Tx) (bool, error)) error {
	for range 100 {
		did := false
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			var err error
			did, err = step(tx)
			return err
		})
		if err != nil || !did {
			return err
		}
	}
	return nil
}

// ContractTick places due orders of active contracts and, when simulating, accepts proposals made to external parties
// idle for `delay` (exported for tests).
func (s *Server) ContractTick(ctx context.Context, delay time.Duration) error {
	if err := s.drain(ctx, func(tx pgx.Tx) (bool, error) {
		var id string
		err := tx.QueryRow(ctx, `SELECT id::text FROM supply_contracts WHERE status = 'active' AND next_at <= now() ORDER BY next_at LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id)
		if err == pgx.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, placeContractOrder(ctx, tx, id, nil)
	}); err != nil || !s.SimulateCounterparties {
		return err
	}
	return s.drain(ctx, func(tx pgx.Tx) (bool, error) {
		var id, code, every, pending string
		var runs int
		var proposer *string
		err := tx.QueryRow(ctx, `
			SELECT c.id::text, c.code, c.every, c.runs, pp.name, pr.user_id::text
			FROM supply_contracts c
			JOIN parties pp ON pp.id = CASE c.proposed_by_side WHEN 'buyer' THEN c.supplier_party_id ELSE c.buyer_party_id END
			JOIN parties pr ON pr.id = CASE c.proposed_by_side WHEN 'buyer' THEN c.buyer_party_id ELSE c.supplier_party_id END
			WHERE c.status = 'proposed' AND pp.kind = 'external' AND c.created_at <= now() - $1::interval
			ORDER BY c.created_at LIMIT 1 FOR UPDATE OF c SKIP LOCKED`, delay).Scan(&id, &code, &every, &runs, &pending, &proposer)
		if err == pgx.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE supply_contracts SET status = 'active' WHERE id = $1`, id); err != nil {
			return false, err
		}
		if err := writeAudit(ctx, tx, audit{ActorLabel: pending, Action: fmt.Sprintf("Kontrak %s: accept", code), EntityType: "transaction",
			EntityID: id, EntityLabel: code, Changes: []change{{Field: "status", Before: ptr("proposed"), After: "active"}}}); err != nil {
			return false, err
		}
		if proposer != nil {
			if err := notify(ctx, tx, *proposer, notification{Type: "transaction_update", Title: fmt.Sprintf("%s menyetujui kontrak %s", pending, code),
				Body: fmt.Sprintf("%s, %d order", contractEvery[every], runs), Href: "/app/contracts/" + id}); err != nil {
				return false, err
			}
		}
		return true, nil
	})
}

// TradeCounterpartyTick takes one step for the external side of every trade idle for `delay` (exported for tests).
// Callers gate it on SimulateCounterparties.
func (s *Server) TradeCounterpartyTick(ctx context.Context, delay time.Duration) error {
	// One step per trade per tick: a trade already looked at (acted on, or waiting on the user) is not picked again.
	seen := map[string]bool{}
	return s.drain(ctx, func(tx pgx.Tx) (bool, error) {
		var id, side, name string
		err := tx.QueryRow(ctx, `
			SELECT t.id::text, CASE WHEN bp.kind = 'external' THEN 'buyer' ELSE 'supplier' END, CASE WHEN bp.kind = 'external' THEN bp.name ELSE sp.name END
			FROM trades t JOIN parties bp ON bp.id = t.buyer_party_id JOIN parties sp ON sp.id = t.supplier_party_id
			WHERE (bp.kind = 'external' OR sp.kind = 'external') AND t.updated_at <= now() - $1::interval
			  AND (t.status IN ('agreement','invoiced','paid','fulfilling','delivered','accepted')
			       OR (t.status = 'completed' AND NOT EXISTS (SELECT 1 FROM reviews r WHERE r.trade_id = t.id
			             AND r.side = CASE WHEN bp.kind = 'external' THEN 'buyer' ELSE 'supplier' END)))
			  AND NOT (t.id = ANY($2::uuid[]))
			ORDER BY t.updated_at LIMIT 1 FOR UPDATE OF t SKIP LOCKED`, delay, keys(seen)).Scan(&id, &side, &name)
		if err == pgx.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		seen[id] = true
		t, err := lockTrade(ctx, tx, id)
		if err != nil {
			return false, err
		}
		st := t.state()
		open := tradeActions(st, side)
		i := slices.IndexFunc(botPriority, func(a string) bool { return slices.Contains(open, a) })
		if i < 0 {
			return true, nil // not the bot's turn
		}
		in := api.TradeActionInput{Action: api.TradeAction(botPriority[i])}
		switch botPriority[i] {
		case "ship":
			var address string
			if err := tx.QueryRow(ctx, `SELECT delivery_address FROM trades WHERE id = $1`, id).Scan(&address); err != nil {
				return false, err
			}
			in.Shipment = &struct {
				Carrier     string    `json:"carrier"`
				DropPoint   string    `json:"dropPoint"`
				Quantity    float64   `json:"quantity"`
				ScheduledAt time.Time `json:"scheduledAt"`
			}{carrierDflt, address, st.UnscheduledQty, time.Now()}
		case "upload_proof":
			in.File = ptr("surat-jalan-ttd.jpg")
		case "review":
			in.Review = &struct {
				Communication float64 `json:"communication"`
				Quality       float64 `json:"quality"`
				Rating        int     `json:"rating"`
				Text          string  `json:"text"`
				Timeliness    float64 `json:"timeliness"`
			}{5, 5, 5, "Transaksi lancar, terima kasih.", 4}
		}
		return true, applyTradeAction(ctx, tx, id, tradeActor{Side: side, Name: name}, in)
	})
}

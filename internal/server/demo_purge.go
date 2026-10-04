package server

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/analytics"
)

var demoSets = []string{
	`CREATE TEMP TABLE d_users ON COMMIT DROP AS SELECT id FROM users WHERE email LIKE $1`,
	`CREATE TEMP TABLE d_orgs ON COMMIT DROP AS SELECT id FROM orgs WHERE created_by IN (SELECT id FROM d_users)`,
	`CREATE TEMP TABLE d_parties ON COMMIT DROP AS SELECT id FROM parties WHERE user_id IN (SELECT id FROM d_users) OR org_id IN (SELECT id FROM d_orgs)`,
	`CREATE TEMP TABLE d_markets ON COMMIT DROP AS SELECT id FROM markets WHERE maker_party_id IN (SELECT id FROM d_parties) OR created_by IN (SELECT id FROM d_users)`,
	`CREATE TEMP TABLE d_auctions ON COMMIT DROP AS SELECT id FROM auctions WHERE market_id IN (SELECT id FROM d_markets)
	   OR owner_user_id IN (SELECT id FROM d_users) OR owner_org_id IN (SELECT id FROM d_orgs)`,
	`CREATE TEMP TABLE d_listings ON COMMIT DROP AS SELECT id FROM listings WHERE owner_party_id IN (SELECT id FROM d_parties)`,
	`CREATE TEMP TABLE d_trades ON COMMIT DROP AS SELECT id FROM trades WHERE buyer_party_id IN (SELECT id FROM d_parties)
	   OR supplier_party_id IN (SELECT id FROM d_parties) OR market_id IN (SELECT id FROM d_markets) OR auction_id IN (SELECT id FROM d_auctions)
	   OR source_listing_id IN (SELECT id FROM d_listings)`,
	`CREATE TEMP TABLE d_rfqs ON COMMIT DROP AS SELECT id FROM rfqs WHERE buyer_party_id IN (SELECT id FROM d_parties)
	   OR source_trade_id IN (SELECT id FROM d_trades) OR source_listing_id IN (SELECT id FROM d_listings)`,
	`INSERT INTO d_trades SELECT t.id FROM trades t JOIN quotes q ON q.id = t.source_quote_id
	   WHERE (q.rfq_id IN (SELECT id FROM d_rfqs) OR q.supplier_party_id IN (SELECT id FROM d_parties)) AND t.id NOT IN (SELECT id FROM d_trades)`,
	`INSERT INTO d_rfqs SELECT id FROM rfqs WHERE source_trade_id IN (SELECT id FROM d_trades) AND id NOT IN (SELECT id FROM d_rfqs)`,
	`CREATE TEMP TABLE d_withdrawals ON COMMIT DROP AS SELECT id FROM withdrawals WHERE party_id IN (SELECT id FROM d_parties)`,
	`CREATE TEMP TABLE d_pools ON COMMIT DROP AS SELECT id FROM collective_pools WHERE created_by IN (SELECT id FROM d_users)
	   OR formed_by IN (SELECT id FROM d_users) OR market_id IN (SELECT id FROM d_markets)`,
	`CREATE TEMP TABLE d_settlements ON COMMIT DROP AS SELECT id FROM settlements WHERE auction_id IN (SELECT id FROM d_auctions)
	   OR winner_party_id IN (SELECT id FROM d_parties)`,
	`CREATE TEMP TABLE d_suppliers ON COMMIT DROP AS SELECT id FROM suppliers WHERE party_id IN (SELECT id FROM d_parties)`,
	`CREATE TEMP TABLE d_opps ON COMMIT DROP AS SELECT o.id FROM opportunities o
	   WHERE EXISTS (SELECT 1 FROM opportunity_listings ol WHERE ol.opportunity_id = o.id)
	     AND NOT EXISTS (SELECT 1 FROM opportunity_listings ol WHERE ol.opportunity_id = o.id AND ol.listing_id NOT IN (SELECT id FROM d_listings))`,
	`CREATE TEMP TABLE d_journals ON COMMIT DROP AS SELECT DISTINCT journal_id AS id FROM ledger_entries
	   WHERE trade_id IN (SELECT id FROM d_trades) OR withdrawal_id IN (SELECT id FROM d_withdrawals)
	      OR account_id IN (SELECT id FROM ledger_accounts WHERE owner_party_id IN (SELECT id FROM d_parties))`,
	`CREATE TEMP TABLE d_ids ON COMMIT DROP AS
	   SELECT id::text AS id FROM d_users UNION SELECT id::text FROM d_orgs UNION SELECT id::text FROM d_parties UNION SELECT id::text FROM d_markets
	   UNION SELECT id::text FROM d_auctions UNION SELECT id::text FROM d_listings UNION SELECT id::text FROM d_trades UNION SELECT id::text FROM d_rfqs
	   UNION SELECT id::text FROM d_withdrawals UNION SELECT id::text FROM d_pools UNION SELECT id::text FROM d_opps
	   UNION SELECT id::text FROM disputes WHERE trade_id IN (SELECT id FROM d_trades)
	   UNION SELECT id::text FROM org_auctions WHERE org_id IN (SELECT id FROM d_orgs)
	   UNION SELECT id::text FROM procurement_requests WHERE org_id IN (SELECT id FROM d_orgs)
	   UNION SELECT id::text FROM org_purchase_history WHERE org_id IN (SELECT id FROM d_orgs) OR supplier_id IN (SELECT id FROM d_suppliers)`,
}

var demoDeletes = []string{
	`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_no_update`,
	`DELETE FROM ledger_entries WHERE journal_id IN (SELECT id FROM d_journals)`,
	`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_no_update`,
	`ALTER TABLE audit_log DISABLE TRIGGER audit_log_no_update`,
	`DELETE FROM audit_log WHERE actor_user_id IN (SELECT id FROM d_users) OR org_id IN (SELECT id FROM d_orgs) OR market_id IN (SELECT id FROM d_markets)
	   OR entity_id IN (SELECT id FROM d_ids)`,
	`ALTER TABLE audit_log ENABLE TRIGGER audit_log_no_update`,
	`DELETE FROM notifications WHERE user_id IN (SELECT id FROM d_users) OR substring(href FROM '[0-9a-f]{8}-[0-9a-f-]{27}') IN (SELECT id FROM d_ids)`,
	`DELETE FROM outbox WHERE payload->>'demo' = 'true' OR aggregate_id IN (SELECT id FROM d_ids)
	   OR substring(aggregate_id FROM '[0-9a-f]{8}-[0-9a-f-]{27}') IN (SELECT id FROM d_ids)`,
	`DELETE FROM purchase_orders WHERE org_id IN (SELECT id FROM d_orgs)`,
	`DELETE FROM org_awards WHERE org_auction_id IN (SELECT id FROM org_auctions WHERE org_id IN (SELECT id FROM d_orgs))`,
	`DELETE FROM org_auctions WHERE org_id IN (SELECT id FROM d_orgs)`,
	`DELETE FROM org_auction_invites WHERE supplier_id IN (SELECT id FROM d_suppliers)`,
	`DELETE FROM org_procurement_invites WHERE supplier_id IN (SELECT id FROM d_suppliers)`,
	`UPDATE org_auction_offers SET supplier_id = NULL WHERE supplier_id IN (SELECT id FROM d_suppliers)`,
	`UPDATE procurement_requests SET pool_id = NULL, status = CASE WHEN status = 'in_collective' THEN 'approved' ELSE status END
	   WHERE pool_id IN (SELECT id FROM d_pools) AND org_id NOT IN (SELECT id FROM d_orgs)`,
	`DELETE FROM procurement_requests WHERE org_id IN (SELECT id FROM d_orgs)`,
	`UPDATE pool_members SET settlement_line_id = NULL WHERE settlement_line_id IN (SELECT id FROM settlement_lines
	   WHERE settlement_id IN (SELECT id FROM d_settlements) OR trade_id IN (SELECT id FROM d_trades))`,
	`DELETE FROM pool_members WHERE pool_id IN (SELECT id FROM d_pools) OR org_id IN (SELECT id FROM d_orgs)`,
	`DELETE FROM collective_pools WHERE id IN (SELECT id FROM d_pools)`,
	`DELETE FROM settlement_lines WHERE settlement_id IN (SELECT id FROM d_settlements) OR trade_id IN (SELECT id FROM d_trades)`,
	`DELETE FROM settlements WHERE id IN (SELECT id FROM d_settlements)`,
	`DELETE FROM contract_orders WHERE trade_id IN (SELECT id FROM d_trades)`,
	`DELETE FROM supply_contracts WHERE buyer_party_id IN (SELECT id FROM d_parties) OR supplier_party_id IN (SELECT id FROM d_parties)
	   OR source_trade_id IN (SELECT id FROM d_trades)`,
	`DELETE FROM market_disputes WHERE market_id IN (SELECT id FROM d_markets)
	   OR escalated_to IN (SELECT id FROM disputes WHERE trade_id IN (SELECT id FROM d_trades))`,
	`DELETE FROM disputes WHERE trade_id IN (SELECT id FROM d_trades)`,
	`UPDATE shipments SET logistics_rfq_id = NULL WHERE logistics_rfq_id IN (SELECT id FROM d_rfqs)`,
	`UPDATE trades SET source_quote_id = NULL WHERE id IN (SELECT id FROM d_trades) AND source_quote_id IS NOT NULL`,
	`DELETE FROM quotes WHERE supplier_party_id IN (SELECT id FROM d_parties) AND rfq_id NOT IN (SELECT id FROM d_rfqs)`,
	`DELETE FROM rfq_invitations WHERE party_id IN (SELECT id FROM d_parties)`,
	`CREATE TEMP TABLE d_convs ON COMMIT DROP AS SELECT conversation_id AS id FROM conversation_participants WHERE party_id IN (SELECT id FROM d_parties)
	   UNION SELECT conversation_id FROM rfqs WHERE id IN (SELECT id FROM d_rfqs)
	   UNION SELECT id FROM conversations WHERE link_id IN (SELECT id FROM d_trades) OR link_id IN (SELECT id FROM d_rfqs)`,
	`DELETE FROM rfqs WHERE id IN (SELECT id FROM d_rfqs)`,
	`DELETE FROM conversations WHERE id IN (SELECT id FROM d_convs)`,
	`DELETE FROM trades WHERE id IN (SELECT id FROM d_trades)`,
	`DELETE FROM withdrawals WHERE id IN (SELECT id FROM d_withdrawals)`,
	`DELETE FROM bank_accounts WHERE party_id IN (SELECT id FROM d_parties)`,
	`UPDATE markets SET opportunity_id = NULL WHERE opportunity_id IN (SELECT id FROM d_opps) AND id NOT IN (SELECT id FROM d_markets)`,
	`DELETE FROM mm_pipeline WHERE market_id IN (SELECT id FROM d_markets)`,
	`UPDATE mm_pipeline SET updated_by = NULL WHERE updated_by IN (SELECT id FROM d_users)`,
	`DELETE FROM opportunities WHERE id IN (SELECT id FROM d_opps)`,
	`DELETE FROM opportunity_participants WHERE party_id IN (SELECT id FROM d_parties)`,
	`DELETE FROM auction_awards WHERE auction_id IN (SELECT id FROM d_auctions)`,
	`UPDATE listings SET market_id = NULL, status = CASE WHEN status = 'in_market' AND auction_id IS NULL
	   THEN CASE kind WHEN 'supply' THEN 'available' ELSE 'open' END ELSE status END
	   WHERE market_id IN (SELECT id FROM d_markets) AND id NOT IN (SELECT id FROM d_listings)`,
	`DELETE FROM listings WHERE id IN (SELECT id FROM d_listings)`,
	`DELETE FROM auctions WHERE id IN (SELECT id FROM d_auctions)`,
	`UPDATE disputes SET market_id = NULL WHERE market_id IN (SELECT id FROM d_markets)`,
	`DELETE FROM markets WHERE id IN (SELECT id FROM d_markets)`,
	`DELETE FROM market_participants WHERE party_id IN (SELECT id FROM d_parties)`,
	`DELETE FROM market_reports WHERE reporter_party_id IN (SELECT id FROM d_parties)`,
	`DELETE FROM user_reports WHERE reporter_party_id IN (SELECT id FROM d_parties)`,
	`DELETE FROM org_purchase_history WHERE supplier_id IN (SELECT id FROM d_suppliers) OR org_id IN (SELECT id FROM d_orgs)`,
	`DELETE FROM suppliers WHERE id IN (SELECT id FROM d_suppliers)`,
	`DELETE FROM ledger_accounts WHERE owner_party_id IN (SELECT id FROM d_parties)`,
	`DELETE FROM conversation_participants WHERE party_id IN (SELECT id FROM d_parties)`,
	`DELETE FROM parties WHERE id IN (SELECT id FROM d_parties)`,
	`DELETE FROM verification_requests WHERE submitted_by IN (SELECT id FROM d_users) OR org_id IN (SELECT id FROM d_orgs)`,
	`DELETE FROM orgs WHERE id IN (SELECT id FROM d_orgs)`,
	`DELETE FROM identities WHERE user_id IN (SELECT id FROM d_users)`,
	`UPDATE identities SET identity_verified_at = NULL, identity_verified_by = NULL, nik_hash = NULL WHERE identity_verified_by IN (SELECT id FROM d_users)`,
	`DELETE FROM users WHERE id IN (SELECT id FROM d_users)`,
}

func (s *Server) PurgeDemo(ctx context.Context) (analytics.DemoIDs, map[string]int, error) {
	var ids analytics.DemoIDs
	counts := map[string]int{}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, engineLockKey); err != nil {
			return err
		}
		for i, q := range demoSets {
			var args []any
			if i == 0 {
				args = []any{demoEmailLike}
			}
			if _, err := tx.Exec(ctx, q, args...); err != nil {
				return fmt.Errorf("%s: %w", q[:40], err)
			}
		}
		for dst, q := range map[*[]string]string{
			&ids.Parties: `SELECT id::text FROM d_parties`, &ids.Orgs: `SELECT id::text FROM d_orgs`, &ids.Markets: `SELECT id::text FROM d_markets`,
			&ids.Auctions: `SELECT id::text FROM d_auctions`, &ids.Trades: `SELECT id::text FROM d_trades`, &ids.Listings: `SELECT id::text FROM d_listings`,
			&ids.Other: `SELECT id FROM d_ids`,
			&ids.Titles: `SELECT 'Auction ditutup: ' || title FROM auctions WHERE id IN (SELECT id FROM d_auctions)
				UNION SELECT 'Auction dimulai: ' || title FROM auctions WHERE id IN (SELECT id FROM d_auctions)
				UNION SELECT 'Bid baru di auction ' || title FROM auctions WHERE id IN (SELECT id FROM d_auctions)
				UNION SELECT 'Transaksi selesai: ' || title FROM trades WHERE id IN (SELECT id FROM d_trades)
				UNION SELECT 'Opportunity baru: ' || title FROM opportunities WHERE id IN (SELECT id FROM d_opps)`,
		} {
			rows, err := tx.Query(ctx, q)
			if err != nil {
				return err
			}
			if *dst, err = collectStrings(rows); err != nil {
				return err
			}
		}
		for _, q := range demoDeletes {
			tag, err := tx.Exec(ctx, q)
			if err != nil {
				return fmt.Errorf("%s: %w", q[:min(60, len(q))], err)
			}
			if tag.Delete() {
				counts[deleteTable(q)] += int(tag.RowsAffected())
			}
		}
		var left int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE email LIKE $1`, demoEmailLike).Scan(&left); err != nil {
			return err
		}
		if left > 0 {
			return fmt.Errorf("%d demo accounts left after purge", left)
		}
		return nil
	})
	return ids, counts, err
}

func deleteTable(q string) string {
	var t string
	_, _ = fmt.Sscanf(q, "DELETE FROM %s", &t)
	return t
}

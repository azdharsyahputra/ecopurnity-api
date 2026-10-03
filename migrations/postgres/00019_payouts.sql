-- +goose Up
-- Manual payouts: an admin transfers a withdrawal from the company bank account by hand and records it (mark paid with
-- the transfer reference) or rejects it (the money goes back to the wallet).
--
-- Ledger: a requested withdrawal no longer leaves bank_clearing (money at the bank) straight away. It moves from the
-- party's wallet to the platform liability payout_pending (owed to sellers, waiting for a manual transfer); mark paid
-- clears payout_pending against bank_clearing (the money left the bank), reject moves it back to the wallet.
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_kind_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_kind_check CHECK (kind IN ('wallet_available','escrow','receivable',
  'platform_revenue','ppn_payable','maker_commission','bank_clearing','payout_pending'));
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_check CHECK (kind = 'ppn_payable'
  OR (kind IN ('platform_revenue','bank_clearing','payout_pending') AND owner_party_id IS NULL)
  OR (kind IN ('wallet_available','escrow','receivable','maker_commission') AND owner_party_id IS NOT NULL));

-- Withdrawals still processing were booked wallet -> bank_clearing: reclassify them to payout_pending so every open
-- withdrawal is settled the same way.
INSERT INTO ledger_accounts (owner_party_id, kind) VALUES (NULL, 'bank_clearing'), (NULL, 'payout_pending') ON CONFLICT DO NOTHING;
WITH j AS MATERIALIZED (SELECT gen_random_uuid() AS journal_id, id, amount_idr FROM withdrawals WHERE status = 'processing')
INSERT INTO ledger_entries (journal_id, account_id, amount, kind, label, withdrawal_id)
SELECT j.journal_id, a.id, CASE a.kind WHEN 'bank_clearing' THEN j.amount_idr ELSE -j.amount_idr END, 'withdrawal',
       'Reklasifikasi pencairan diproses', j.id
FROM j CROSS JOIN ledger_accounts a WHERE a.owner_party_id IS NULL AND a.kind IN ('bank_clearing','payout_pending');

ALTER TABLE withdrawals DROP CONSTRAINT withdrawals_status_check;
ALTER TABLE withdrawals DROP CONSTRAINT withdrawals_check;
ALTER TABLE withdrawals RENAME COLUMN failure_reason TO reject_reason;
UPDATE withdrawals SET status = 'rejected', reject_reason = coalesce(reject_reason, 'Pencairan gagal') WHERE status = 'failed';
ALTER TABLE withdrawals
  ADD COLUMN code text UNIQUE DEFAULT next_code('WDR'),
  ADD COLUMN transfer_ref text CHECK (length(btrim(transfer_ref)) > 0),   -- bank transfer reference the admin typed
  ADD COLUMN admin_note   text,
  ADD COLUMN decided_by   uuid REFERENCES users(id),                      -- the admin who marked it paid or rejected it
  ADD COLUMN decided_at   timestamptz;                                     -- when it was recorded (paid_at: when transferred)
UPDATE withdrawals SET decided_at = coalesce(paid_at, updated_at) WHERE status <> 'processing';
ALTER TABLE withdrawals ALTER COLUMN code SET NOT NULL;
ALTER TABLE withdrawals
  ADD CONSTRAINT withdrawals_status_check CHECK (status IN ('processing','paid','rejected')),
  ADD CONSTRAINT withdrawals_decided_check CHECK ((status = 'processing') = (decided_at IS NULL)),
  ADD CONSTRAINT withdrawals_paid_check CHECK (status <> 'paid' OR paid_at IS NOT NULL),
  ADD CONSTRAINT withdrawals_rejected_check CHECK (status <> 'rejected' OR reject_reason IS NOT NULL);
CREATE INDEX withdrawals_decided_idx ON withdrawals (decided_at DESC) WHERE status <> 'processing';

-- +goose Down
DROP INDEX withdrawals_decided_idx;
ALTER TABLE withdrawals DROP CONSTRAINT withdrawals_status_check, DROP CONSTRAINT withdrawals_decided_check,
  DROP CONSTRAINT withdrawals_paid_check, DROP CONSTRAINT withdrawals_rejected_check;
ALTER TABLE withdrawals DROP COLUMN code, DROP COLUMN transfer_ref, DROP COLUMN admin_note, DROP COLUMN decided_by, DROP COLUMN decided_at;
UPDATE withdrawals SET status = 'failed' WHERE status = 'rejected';
ALTER TABLE withdrawals RENAME COLUMN reject_reason TO failure_reason;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_status_check CHECK (status IN ('processing','paid','failed')),
  ADD CONSTRAINT withdrawals_check CHECK (status <> 'paid' OR paid_at IS NOT NULL);

-- The ledger is append-only; for the rollback only, fold payout_pending back into bank_clearing (every journal stays
-- balanced: both are platform accounts).
ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_no_update;
UPDATE ledger_entries SET account_id = (SELECT id FROM ledger_accounts WHERE owner_party_id IS NULL AND kind = 'bank_clearing')
WHERE account_id = (SELECT id FROM ledger_accounts WHERE owner_party_id IS NULL AND kind = 'payout_pending');
ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_no_update;
DELETE FROM ledger_accounts WHERE kind = 'payout_pending';
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_check CHECK (kind = 'ppn_payable'
  OR (kind IN ('platform_revenue','bank_clearing') AND owner_party_id IS NULL)
  OR (kind IN ('wallet_available','escrow','receivable','maker_commission') AND owner_party_id IS NOT NULL));
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_kind_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_kind_check CHECK (kind IN ('wallet_available','escrow','receivable',
  'platform_revenue','ppn_payable','maker_commission','bank_clearing'));

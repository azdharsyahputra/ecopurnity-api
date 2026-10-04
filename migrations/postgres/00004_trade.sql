-- +goose Up

CREATE TABLE trades (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code              text NOT NULL UNIQUE DEFAULT next_code('TRX'),
  title             text NOT NULL CHECK (length(btrim(title)) > 0),
  buyer_party_id    uuid NOT NULL REFERENCES parties(id),
  supplier_party_id uuid NOT NULL REFERENCES parties(id),
  quantity          qty NOT NULL CHECK (quantity > 0),
  unit              text NOT NULL,
  unit_price_idr    idr NOT NULL,
  total_idr         idr NOT NULL,
  terms             text NOT NULL DEFAULT 'escrow' CHECK (terms IN ('escrow','net14','net30')),
  status            text NOT NULL DEFAULT 'agreement'
                    CHECK (status IN ('agreement','invoiced','paid','fulfilling','delivered','accepted','completed','cancelled','disputed')),
  maker_fee_rate    numeric(6,5) NOT NULL DEFAULT 0 CHECK (maker_fee_rate >= 0 AND maker_fee_rate < 1),
  platform_fee_rate numeric(6,5) NOT NULL DEFAULT 0.01 CHECK (platform_fee_rate >= 0 AND platform_fee_rate < 1),
  market_id         uuid REFERENCES markets(id),
  auction_id        uuid REFERENCES auctions(id),
  source_listing_id uuid REFERENCES listings(id),
  source_quote_id   uuid,
  group_label       text,
  group_share       numeric(6,5) CHECK (group_share > 0 AND group_share <= 1),
  delivery_address  text NOT NULL,
  due_at            timestamptz NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CHECK (buyer_party_id <> supplier_party_id),
  CHECK (total_idr = round(quantity * unit_price_idr)),
  CHECK ((group_label IS NULL) = (group_share IS NULL))
);
CREATE TRIGGER trades_updated_at BEFORE UPDATE ON trades FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE INDEX trades_buyer_idx ON trades (buyer_party_id, created_at DESC);
CREATE INDEX trades_supplier_idx ON trades (supplier_party_id, created_at DESC);
CREATE INDEX trades_market_idx ON trades (market_id) WHERE market_id IS NOT NULL;
CREATE INDEX trades_auction_idx ON trades (auction_id) WHERE auction_id IS NOT NULL;
CREATE INDEX trades_listing_idx ON trades (source_listing_id) WHERE source_listing_id IS NOT NULL;
CREATE UNIQUE INDEX trades_quote_key ON trades (source_quote_id) WHERE source_quote_id IS NOT NULL;
COMMENT ON TABLE trades IS 'One two-sided trade (buyer party, supplier party); each side''s Transaction is a projection of this row.';

CREATE TABLE trade_acceptances (
  trade_id     uuid NOT NULL REFERENCES trades(id) ON DELETE CASCADE,
  side         text NOT NULL CHECK (side IN ('buyer','supplier')),
  accepted_at  timestamptz NOT NULL DEFAULT now(),
  accepted_by  uuid REFERENCES users(id),
  PRIMARY KEY (trade_id, side)
);
COMMENT ON TABLE trade_acceptances IS 'One side''s acceptance of a trade agreement.';

CREATE TABLE trade_events (
  id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  trade_id  uuid NOT NULL REFERENCES trades(id) ON DELETE CASCADE,
  status    text NOT NULL
            CHECK (status IN ('agreement','invoiced','paid','fulfilling','delivered','accepted','completed','cancelled','disputed')),
  at        timestamptz NOT NULL DEFAULT now(),
  note      text,
  actor_user_id uuid REFERENCES users(id)
);
CREATE INDEX trade_events_trade_idx ON trade_events (trade_id, at);
COMMENT ON TABLE trade_events IS 'A status the trade reached, with when and why (TransactionDetail.timeline).';

CREATE TABLE trade_documents (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  trade_id    uuid NOT NULL REFERENCES trades(id) ON DELETE CASCADE,
  kind        text NOT NULL CHECK (kind IN ('order','agreement','invoice','proof','other')),
  name        text NOT NULL,
  object_key  text,
  uploaded_by uuid REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX trade_documents_trade_idx ON trade_documents (trade_id, created_at);
COMMENT ON TABLE trade_documents IS 'A document attached to a trade: purchase order, agreement, invoice, delivery proof.';

CREATE TABLE invoices (
  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  trade_id              uuid NOT NULL UNIQUE REFERENCES trades(id) ON DELETE CASCADE,
  number                text NOT NULL UNIQUE,
  issued_at             timestamptz NOT NULL DEFAULT now(),
  due_at                timestamptz NOT NULL,
  subtotal_idr          idr NOT NULL,
  ppn_idr               idr NOT NULL,
  buyer_pays_idr        idr NOT NULL,
  platform_fee_idr      idr NOT NULL,
  maker_fee_idr         idr NOT NULL,
  supplier_receives_idr idr NOT NULL,
  status                text NOT NULL DEFAULT 'unpaid' CHECK (status IN ('unpaid','escrow','released','refunded')),
  paid_at               timestamptz,
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CHECK (buyer_pays_idr = subtotal_idr + ppn_idr),
  CHECK (supplier_receives_idr = buyer_pays_idr - platform_fee_idr - maker_fee_idr),
  CHECK (status = 'unpaid' OR paid_at IS NOT NULL)
);
CREATE TRIGGER invoices_updated_at BEFORE UPDATE ON invoices FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE invoices IS 'The invoice of a trade with its frozen money breakdown and payment state.';

CREATE TABLE qc_results (
  trade_id          uuid PRIMARY KEY REFERENCES trades(id) ON DELETE CASCADE,
  outcome           text NOT NULL CHECK (outcome IN ('accepted','partial','rejected')),
  accepted_quantity qty NOT NULL,
  note              text,
  at                timestamptz NOT NULL DEFAULT now(),
  checked_by        uuid REFERENCES users(id),
  CHECK (outcome = 'accepted' OR length(btrim(coalesce(note, ''))) > 0),
  CHECK (outcome <> 'rejected' OR accepted_quantity = 0),
  CHECK (outcome <> 'partial' OR accepted_quantity > 0)
);
COMMENT ON TABLE qc_results IS 'The buyer''s receipt inspection of a trade.';

CREATE TABLE reviews (
  trade_id      uuid NOT NULL REFERENCES trades(id) ON DELETE CASCADE,
  side          text NOT NULL CHECK (side IN ('buyer','supplier')),
  rating        smallint NOT NULL CHECK (rating BETWEEN 1 AND 5),
  quality       numeric(2,1) NOT NULL CHECK (quality BETWEEN 1 AND 5),
  timeliness    numeric(2,1) NOT NULL CHECK (timeliness BETWEEN 1 AND 5),
  communication numeric(2,1) NOT NULL CHECK (communication BETWEEN 1 AND 5),
  text          text NOT NULL,
  reviewed_by   uuid REFERENCES users(id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (trade_id, side)
);
COMMENT ON TABLE reviews IS 'One side''s review of the other after a completed trade.';

CREATE TABLE disputes (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code              text NOT NULL UNIQUE DEFAULT next_code('DSP'),
  trade_id          uuid NOT NULL REFERENCES trades(id),
  status            text NOT NULL DEFAULT 'open' CHECK (status IN ('open','evidence','review','resolved')),
  reason            text NOT NULL CHECK (length(btrim(reason)) > 0),
  opened_by_side    text CHECK (opened_by_side IN ('buyer','supplier')),
  opened_by         uuid REFERENCES users(id),
  opened_at         timestamptz NOT NULL DEFAULT now(),
  market_id         uuid REFERENCES markets(id),
  resolution_kind   text CHECK (resolution_kind IN ('refund','release','partial')),
  refund_idr        idr,
  release_idr       idr,
  resolution_reason text,
  resolved_at       timestamptz,
  resolved_by       uuid REFERENCES users(id),
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CHECK ((status = 'resolved') = (resolution_kind IS NOT NULL)),
  CHECK (resolution_kind IS NULL OR (refund_idr IS NOT NULL AND release_idr IS NOT NULL AND resolved_at IS NOT NULL
                                     AND length(btrim(coalesce(resolution_reason, ''))) > 0)),
  CHECK (resolution_kind <> 'refund' OR release_idr = 0),
  CHECK (resolution_kind <> 'release' OR refund_idr = 0),
  CHECK (resolution_kind <> 'partial' OR (refund_idr > 0 AND release_idr > 0))
);
CREATE TRIGGER disputes_updated_at BEFORE UPDATE ON disputes FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE UNIQUE INDEX disputes_one_open_key ON disputes (trade_id) WHERE status <> 'resolved';
CREATE INDEX disputes_trade_idx ON disputes (trade_id);
CREATE INDEX disputes_queue_idx ON disputes (opened_at DESC) WHERE status <> 'resolved';
CREATE INDEX disputes_market_idx ON disputes (market_id, opened_at DESC) WHERE market_id IS NOT NULL;
COMMENT ON TABLE disputes IS 'A dispute case on a trade, handled by admin governance.';

ALTER TABLE market_disputes ADD CONSTRAINT market_disputes_escalated_to_fkey FOREIGN KEY (escalated_to) REFERENCES disputes(id);

CREATE TABLE dispute_evidence (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  dispute_id  uuid NOT NULL REFERENCES disputes(id) ON DELETE CASCADE,
  side        text NOT NULL CHECK (side IN ('buyer','supplier','admin')),
  author_user_id uuid REFERENCES users(id),
  author_name text NOT NULL,
  text        text NOT NULL CHECK (length(btrim(text)) > 0),
  file_name   text,
  file_key    text,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dispute_evidence_dispute_idx ON dispute_evidence (dispute_id, created_at);
COMMENT ON TABLE dispute_evidence IS 'One piece of evidence in a dispute from a party or an admin; the opening reason is the first.';

CREATE TABLE dispute_events (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  dispute_id    uuid NOT NULL REFERENCES disputes(id) ON DELETE CASCADE,
  at            timestamptz NOT NULL DEFAULT now(),
  actor_user_id uuid REFERENCES users(id),
  actor_label   text NOT NULL,
  label         text NOT NULL
);
CREATE INDEX dispute_events_dispute_idx ON dispute_events (dispute_id, at);
COMMENT ON TABLE dispute_events IS 'One step of a dispute case timeline (opened, evidence requested, review, decision).';

CREATE TABLE bank_accounts (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  party_id       uuid NOT NULL REFERENCES parties(id),
  bank           text NOT NULL CHECK (length(btrim(bank)) > 0),
  holder         text NOT NULL CHECK (length(btrim(holder)) > 0),
  account_no_enc bytea NOT NULL,
  account_last4  text NOT NULL CHECK (account_last4 ~ '^[0-9]{4}$'),
  created_by     uuid REFERENCES users(id),
  created_at     timestamptz NOT NULL DEFAULT now(),
  replaced_at    timestamptz
);
CREATE UNIQUE INDEX bank_accounts_active_key ON bank_accounts (party_id) WHERE replaced_at IS NULL;
COMMENT ON TABLE bank_accounts IS 'A party''s payout bank account; the row without replaced_at is the active one.';

CREATE TABLE withdrawals (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  party_id        uuid NOT NULL REFERENCES parties(id),
  bank_account_id uuid NOT NULL REFERENCES bank_accounts(id),
  amount_idr      idr NOT NULL CHECK (amount_idr > 0),
  status          text NOT NULL DEFAULT 'processing' CHECK (status IN ('processing','paid','failed')),
  requested_by    uuid REFERENCES users(id),
  paid_at         timestamptz,
  failure_reason  text,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CHECK (status <> 'paid' OR paid_at IS NOT NULL)
);
CREATE TRIGGER withdrawals_updated_at BEFORE UPDATE ON withdrawals FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE INDEX withdrawals_party_idx ON withdrawals (party_id, created_at DESC);
CREATE INDEX withdrawals_queue_idx ON withdrawals (created_at) WHERE status = 'processing';
COMMENT ON TABLE withdrawals IS 'A payout of available funds to a party''s bank account.';

CREATE TABLE ledger_accounts (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_party_id uuid REFERENCES parties(id),
  kind           text NOT NULL CHECK (kind IN ('wallet_available','escrow','receivable','platform_revenue','ppn_payable',
                                               'maker_commission','bank_clearing')),
  created_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (owner_party_id, kind),
  CHECK (kind = 'ppn_payable'
         OR (kind IN ('platform_revenue','bank_clearing') AND owner_party_id IS NULL)
         OR (kind IN ('wallet_available','escrow','receivable','maker_commission') AND owner_party_id IS NOT NULL))
);
COMMENT ON TABLE ledger_accounts IS 'A ledger account: one per owner party and kind, or a platform account (no owner).';

CREATE TABLE ledger_entries (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  journal_id    uuid NOT NULL,
  account_id    uuid NOT NULL REFERENCES ledger_accounts(id),
  amount        bigint NOT NULL CHECK (amount <> 0),
  kind          text NOT NULL CHECK (kind IN ('escrow','payout','refund','payment','withdrawal','fee')),
  label         text NOT NULL,
  trade_id      uuid REFERENCES trades(id),
  withdrawal_id uuid REFERENCES withdrawals(id),
  created_by    uuid REFERENCES users(id),
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id, created_at DESC);
CREATE INDEX ledger_entries_journal_idx ON ledger_entries (journal_id);
CREATE INDEX ledger_entries_trade_idx ON ledger_entries (trade_id) WHERE trade_id IS NOT NULL;
CREATE INDEX ledger_entries_withdrawal_idx ON ledger_entries (withdrawal_id) WHERE withdrawal_id IS NOT NULL;
COMMENT ON TABLE ledger_entries IS 'One debit or credit line of a balanced ledger journal.';

-- +goose StatementBegin
CREATE FUNCTION ledger_journal_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (SELECT sum(amount) FROM ledger_entries WHERE journal_id = NEW.journal_id) <> 0 THEN
    RAISE EXCEPTION 'ledger journal % does not balance', NEW.journal_id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER ledger_entries_balanced AFTER INSERT ON ledger_entries
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_journal_balanced();

-- +goose StatementBegin
CREATE FUNCTION ledger_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'ledger_entries is append-only; post a reversing journal instead';
END $$;
-- +goose StatementEnd
CREATE TRIGGER ledger_entries_no_update BEFORE UPDATE OR DELETE ON ledger_entries FOR EACH ROW EXECUTE FUNCTION ledger_immutable();

CREATE TABLE settlements (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  auction_id      uuid NOT NULL UNIQUE REFERENCES auctions(id),
  side            text NOT NULL CHECK (side IN ('procurement','selling')),
  winner_party_id uuid NOT NULL REFERENCES parties(id),
  price_idr       idr NOT NULL,
  settled_by      uuid NOT NULL REFERENCES users(id),
  settled_at      timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE settlements IS 'The settlement of a collective round: its winning price split back over contributing members.';

CREATE TABLE settlement_lines (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  settlement_id   uuid NOT NULL REFERENCES settlements(id) ON DELETE CASCADE,
  member_party_id uuid NOT NULL REFERENCES parties(id),
  quantity        qty NOT NULL CHECK (quantity > 0),
  share           numeric(6,5) NOT NULL CHECK (share > 0 AND share <= 1),
  amount_idr      idr NOT NULL,
  trade_id        uuid UNIQUE REFERENCES trades(id),
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX settlement_lines_settlement_idx ON settlement_lines (settlement_id);
CREATE INDEX settlement_lines_member_idx ON settlement_lines (member_party_id);
COMMENT ON TABLE settlement_lines IS 'One member''s pro-rata share of a settled collective round.';

CREATE TABLE conversations (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  subject         text NOT NULL CHECK (length(btrim(subject)) > 0),
  link_type       text CHECK (link_type IN ('rfq','match','transaction')),
  link_id         uuid,
  message_seq     bigint NOT NULL DEFAULT 0,
  last_message_at timestamptz NOT NULL DEFAULT now(),
  created_by      uuid REFERENCES users(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  CHECK ((link_type IS NULL) = (link_id IS NULL))
);
CREATE INDEX conversations_link_idx ON conversations (link_type, link_id) WHERE link_id IS NOT NULL;
COMMENT ON TABLE conversations IS 'A chat thread between parties, optionally about an RFQ, match or trade.';

CREATE TABLE conversation_participants (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  conversation_id  uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  party_id         uuid NOT NULL REFERENCES parties(id),
  user_id          uuid REFERENCES users(id) ON DELETE CASCADE,
  joined_at        timestamptz NOT NULL DEFAULT now(),
  last_read_seq    bigint NOT NULL DEFAULT 0,
  last_read_at     timestamptz,
  muted            boolean NOT NULL DEFAULT false,
  UNIQUE NULLS NOT DISTINCT (conversation_id, party_id, user_id)
);
CREATE INDEX conversation_participants_user_idx ON conversation_participants (user_id, conversation_id) WHERE user_id IS NOT NULL;
CREATE INDEX conversation_participants_party_idx ON conversation_participants (party_id, conversation_id);
CREATE INDEX conversations_last_message_idx ON conversations (last_message_at DESC);
COMMENT ON TABLE conversation_participants IS 'A party (and the user behind it) in a conversation, with read state.';

CREATE TABLE messages (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  seq             bigint NOT NULL,
  kind            text NOT NULL DEFAULT 'text' CHECK (kind IN ('text','system','attachment')),
  author_party_id uuid REFERENCES parties(id),
  author_user_id  uuid REFERENCES users(id),
  body            text NOT NULL DEFAULT '',
  attachment_key  text,
  attachment_name text,
  client_msg_id   uuid,
  created_at      timestamptz NOT NULL DEFAULT now(),
  edited_at       timestamptz,
  deleted_at      timestamptz,
  UNIQUE (conversation_id, seq),
  UNIQUE (author_user_id, client_msg_id),
  CHECK (kind = 'system' OR author_party_id IS NOT NULL),
  CHECK (kind <> 'text' OR length(btrim(body)) > 0),
  CHECK (kind <> 'attachment' OR attachment_key IS NOT NULL)
);
COMMENT ON TABLE messages IS 'One message in a conversation, numbered by a per-conversation sequence.';

-- +goose StatementBegin
CREATE FUNCTION messages_assign_seq() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE conversations SET message_seq = message_seq + 1, last_message_at = NEW.created_at
   WHERE id = NEW.conversation_id
  RETURNING message_seq INTO NEW.seq;
  IF NEW.seq IS NULL THEN
    RAISE EXCEPTION 'conversation % not found', NEW.conversation_id USING ERRCODE = 'foreign_key_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER messages_seq BEFORE INSERT ON messages FOR EACH ROW EXECUTE FUNCTION messages_assign_seq();

CREATE TABLE rfqs (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code              text NOT NULL UNIQUE DEFAULT next_code('RFQ'),
  buyer_party_id    uuid NOT NULL REFERENCES parties(id),
  item              text NOT NULL CHECK (length(btrim(item)) > 0),
  category_id       category_id NOT NULL,
  quantity          qty NOT NULL CHECK (quantity > 0),
  unit              text NOT NULL,
  target_price_idr  idr,
  deadline          timestamptz NOT NULL,
  location          text NOT NULL,
  spec              text NOT NULL DEFAULT '',
  status            text NOT NULL DEFAULT 'open' CHECK (status IN ('open','awarded','closed')),
  conversation_id   uuid NOT NULL REFERENCES conversations(id),
  source_kind       text CHECK (source_kind IN ('repeat','listing','match','logistics')),
  source_trade_id   uuid REFERENCES trades(id),
  source_listing_id uuid REFERENCES listings(id),
  source_match_id   uuid REFERENCES matches(id),
  created_by        uuid REFERENCES users(id),
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CHECK (CASE source_kind
           WHEN 'repeat'    THEN source_trade_id IS NOT NULL AND num_nonnulls(source_listing_id, source_match_id) = 0
           WHEN 'logistics' THEN source_trade_id IS NOT NULL AND num_nonnulls(source_listing_id, source_match_id) = 0
           WHEN 'listing'   THEN source_listing_id IS NOT NULL AND num_nonnulls(source_trade_id, source_match_id) = 0
           WHEN 'match'     THEN source_match_id IS NOT NULL AND num_nonnulls(source_trade_id, source_listing_id) = 0
           ELSE num_nonnulls(source_trade_id, source_listing_id, source_match_id) = 0
         END)
);
CREATE TRIGGER rfqs_updated_at BEFORE UPDATE ON rfqs FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE INDEX rfqs_buyer_idx ON rfqs (buyer_party_id, created_at DESC);
CREATE INDEX rfqs_open_category_idx ON rfqs (category_id, created_at DESC) WHERE status = 'open';
CREATE INDEX rfqs_source_trade_idx ON rfqs (source_trade_id) WHERE source_trade_id IS NOT NULL;
COMMENT ON TABLE rfqs IS 'A buyer''s request for quotation.';

CREATE TABLE rfq_invitations (
  rfq_id     uuid NOT NULL REFERENCES rfqs(id) ON DELETE CASCADE,
  party_id   uuid NOT NULL REFERENCES parties(id),
  invited_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (rfq_id, party_id)
);
CREATE INDEX rfq_invitations_party_idx ON rfq_invitations (party_id);
COMMENT ON TABLE rfq_invitations IS 'A supplier party invited to quote on an RFQ.';

CREATE TABLE quotes (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  rfq_id            uuid NOT NULL REFERENCES rfqs(id) ON DELETE CASCADE,
  supplier_party_id uuid NOT NULL REFERENCES parties(id),
  price_idr         idr NOT NULL CHECK (price_idr > 0),
  quantity          qty NOT NULL CHECK (quantity > 0),
  lead_time_days    integer NOT NULL CHECK (lead_time_days >= 0),
  terms             text NOT NULL CHECK (terms IN ('escrow','net14','net30')),
  note              text NOT NULL DEFAULT '',
  status            text NOT NULL DEFAULT 'submitted' CHECK (status IN ('submitted','countered','accepted','declined','withdrawn')),
  counter_price_idr idr CHECK (counter_price_idr > 0),
  created_by        uuid REFERENCES users(id),
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CHECK (status <> 'countered' OR counter_price_idr IS NOT NULL)
);
CREATE TRIGGER quotes_updated_at BEFORE UPDATE ON quotes FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE INDEX quotes_rfq_idx ON quotes (rfq_id, created_at);
CREATE INDEX quotes_supplier_idx ON quotes (supplier_party_id, created_at DESC);
CREATE UNIQUE INDEX quotes_one_active_key ON quotes (rfq_id, supplier_party_id) WHERE status IN ('submitted','countered');
CREATE UNIQUE INDEX quotes_one_accepted_key ON quotes (rfq_id) WHERE status = 'accepted';
COMMENT ON TABLE quotes IS 'A supplier''s priced offer on an RFQ, negotiated by counter and revise.';

ALTER TABLE trades ADD CONSTRAINT trades_source_quote_id_fkey FOREIGN KEY (source_quote_id) REFERENCES quotes(id);

CREATE TABLE quote_events (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  quote_id      uuid NOT NULL REFERENCES quotes(id) ON DELETE CASCADE,
  at            timestamptz NOT NULL DEFAULT now(),
  actor_user_id uuid REFERENCES users(id),
  actor_label   text NOT NULL,
  text          text NOT NULL
);
CREATE INDEX quote_events_quote_idx ON quote_events (quote_id, at);
COMMENT ON TABLE quote_events IS 'One step of a quote''s negotiation history.';

CREATE TABLE shipments (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  trade_id          uuid NOT NULL REFERENCES trades(id) ON DELETE CASCADE,
  quantity          qty NOT NULL CHECK (quantity > 0),
  drop_point        text NOT NULL CHECK (length(btrim(drop_point)) > 0),
  carrier           text NOT NULL DEFAULT 'Armada supplier',
  scheduled_at      timestamptz NOT NULL DEFAULT now(),
  status            text NOT NULL DEFAULT 'scheduled' CHECK (status IN ('scheduled','in_transit','delivered')),
  delivered_at      timestamptz,
  proof_document_id uuid REFERENCES trade_documents(id),
  logistics_rfq_id  uuid REFERENCES rfqs(id),
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);
CREATE TRIGGER shipments_updated_at BEFORE UPDATE ON shipments FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE INDEX shipments_trade_idx ON shipments (trade_id, scheduled_at);
CREATE INDEX shipments_logistics_rfq_idx ON shipments (logistics_rfq_id) WHERE logistics_rfq_id IS NOT NULL;
COMMENT ON TABLE shipments IS 'One staged delivery of part of a trade''s quantity.';

CREATE TABLE supply_contracts (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code              text NOT NULL UNIQUE DEFAULT next_code('CTR'),
  buyer_party_id    uuid NOT NULL REFERENCES parties(id),
  supplier_party_id uuid NOT NULL REFERENCES parties(id),
  item              text NOT NULL CHECK (length(btrim(item)) > 0),
  quantity          qty NOT NULL CHECK (quantity > 0),
  unit              text NOT NULL,
  unit_price_idr    idr NOT NULL,
  terms             text NOT NULL DEFAULT 'escrow' CHECK (terms IN ('escrow','net14','net30')),
  every             text NOT NULL CHECK (every IN ('weekly','biweekly','monthly')),
  runs              smallint NOT NULL CHECK (runs BETWEEN 2 AND 52),
  next_at           timestamptz NOT NULL,
  status            text NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed','active','paused','ended','declined')),
  proposed_by_side  text NOT NULL CHECK (proposed_by_side IN ('buyer','supplier')),
  source_trade_id   uuid REFERENCES trades(id),
  created_by        uuid REFERENCES users(id),
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CHECK (buyer_party_id <> supplier_party_id)
);
CREATE TRIGGER supply_contracts_updated_at BEFORE UPDATE ON supply_contracts FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE INDEX supply_contracts_buyer_idx ON supply_contracts (buyer_party_id, created_at DESC);
CREATE INDEX supply_contracts_supplier_idx ON supply_contracts (supplier_party_id, created_at DESC);
CREATE INDEX supply_contracts_due_idx ON supply_contracts (next_at) WHERE status = 'active';
COMMENT ON TABLE supply_contracts IS 'A standing supply contract: the same order re-placed every period.';

CREATE TABLE contract_orders (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  contract_id uuid NOT NULL REFERENCES supply_contracts(id) ON DELETE CASCADE,
  run         smallint NOT NULL CHECK (run BETWEEN 1 AND 52),
  trade_id    uuid NOT NULL UNIQUE REFERENCES trades(id),
  placed_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (contract_id, run)
);
COMMENT ON TABLE contract_orders IS 'One run of a supply contract and the trade it placed.';

ALTER TABLE matches ADD CONSTRAINT matches_conversation_id_fkey
  FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE matches DROP CONSTRAINT matches_conversation_id_fkey;
DROP TABLE contract_orders;
DROP TABLE supply_contracts;
DROP TABLE shipments;
DROP TABLE quote_events;
ALTER TABLE trades DROP CONSTRAINT trades_source_quote_id_fkey;
DROP TABLE quotes;
DROP TABLE rfq_invitations;
DROP TABLE rfqs;
DROP TRIGGER messages_seq ON messages;
DROP FUNCTION messages_assign_seq();
DROP TABLE messages;
DROP TABLE conversation_participants;
DROP TABLE conversations;
DROP TABLE settlement_lines;
DROP TABLE settlements;
DROP TRIGGER ledger_entries_no_update ON ledger_entries;
DROP FUNCTION ledger_immutable();
DROP TRIGGER ledger_entries_balanced ON ledger_entries;
DROP FUNCTION ledger_journal_balanced();
DROP TABLE ledger_entries;
DROP TABLE ledger_accounts;
DROP TABLE withdrawals;
DROP TABLE bank_accounts;
DROP TABLE dispute_events;
DROP TABLE dispute_evidence;
ALTER TABLE market_disputes DROP CONSTRAINT market_disputes_escalated_to_fkey;
DROP TABLE disputes;
DROP TABLE reviews;
DROP TABLE qc_results;
DROP TABLE invoices;
DROP TABLE trade_documents;
DROP TABLE trade_events;
DROP TABLE trade_acceptances;
DROP TABLE trades;

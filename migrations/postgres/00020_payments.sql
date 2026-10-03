-- +goose Up
-- Gateway payments (Midtrans Core API): one row per attempt to pay a trade's invoice. The money itself moves in the
-- ledger when the gateway reports settlement (the engine's `pay` step); this table only tracks the attempts.
CREATE TABLE payments (
  id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  trade_id               uuid NOT NULL REFERENCES trades(id) ON DELETE CASCADE,
  invoice_id             uuid NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
  payer_party_id         uuid NOT NULL REFERENCES parties(id),   -- the buyer side that started it
  payer_label            text NOT NULL,                          -- actor label for the trade history ("Nama (Finance)")
  created_by             uuid NOT NULL REFERENCES users(id),
  method                 text NOT NULL CHECK (method IN ('bank_transfer','echannel','qris','gopay','shopeepay')),
  bank                   text CHECK (bank IN ('bca','bni','bri','permata','cimb','mandiri')),  -- VA bank; mandiri for echannel
  order_id               text NOT NULL UNIQUE,                   -- gateway order id, unique per attempt
  gross_amount_idr       idr NOT NULL CHECK (gross_amount_idr > 0),
  status                 text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','settlement','expire','cancel','deny','failure')),
  gateway_transaction_id text,
  va_number              text,
  biller_code            text,
  bill_key               text,
  qr_url                 text,
  deeplink_url           text,
  expires_at             timestamptz NOT NULL,
  paid_at                timestamptz,
  last_notification      jsonb,                                  -- the gateway's last notification, without signature_key
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  CHECK ((method IN ('bank_transfer','echannel')) = (bank IS NOT NULL)),
  CHECK ((status = 'settlement') = (paid_at IS NOT NULL))
);
-- One open attempt per invoice: a new one cancels the old one at the gateway first.
CREATE UNIQUE INDEX payments_one_pending_per_invoice ON payments (invoice_id) WHERE status = 'pending';
CREATE INDEX payments_trade_idx ON payments (trade_id, created_at DESC);
CREATE INDEX payments_pending_idx ON payments (updated_at) WHERE status = 'pending';
CREATE TRIGGER payments_updated_at BEFORE UPDATE ON payments FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE payments IS 'An attempt to pay a trade invoice through the payment gateway (Midtrans), with its instructions and outcome.';

-- +goose Down
DROP TABLE payments;

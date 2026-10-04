-- +goose Up
CREATE TABLE opportunity_listings (
  opportunity_id  uuid NOT NULL REFERENCES opportunities(id) ON DELETE CASCADE,
  listing_id      uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  party_id        uuid NOT NULL REFERENCES parties(id),
  role            text NOT NULL CHECK (role IN ('buyer','supplier')),
  quantity        qty NOT NULL CHECK (quantity > 0),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (opportunity_id, listing_id)
);
CREATE INDEX opportunity_listings_listing_idx ON opportunity_listings (listing_id);
CREATE TRIGGER opportunity_listings_updated_at BEFORE UPDATE ON opportunity_listings FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE opportunity_listings IS 'An open listing the opportunity engine counted in an opportunity (detected cluster member).';

-- +goose Down
DROP TABLE opportunity_listings;

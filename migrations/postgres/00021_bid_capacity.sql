-- +goose Up
ALTER TABLE bids ADD COLUMN capacity qty CHECK (capacity > 0);
COMMENT ON COLUMN bids.capacity IS 'Quantity the bidder can supply (auction unit); NULL = the whole lot. Owner and bidder only.';

-- +goose Down
ALTER TABLE bids DROP COLUMN capacity;

-- +goose Up
-- How much of the lot a supplier says it can deliver with a bid (reverse and sealed auctions), in the auction's unit.
-- NULL means the whole lot. The owner's evaluation reads it as the offer's capacity and an award line may not exceed it.
-- Never shown to other bidders (only the owner's evaluation and the bidder's own bid).
ALTER TABLE bids ADD COLUMN capacity qty CHECK (capacity > 0);
COMMENT ON COLUMN bids.capacity IS 'Quantity the bidder can supply (auction unit); NULL = the whole lot. Owner and bidder only.';

-- +goose Down
ALTER TABLE bids DROP COLUMN capacity;

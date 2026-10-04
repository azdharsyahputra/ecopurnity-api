-- +goose Up
ALTER TABLE procurement_approvals ADD COLUMN on_behalf boolean NOT NULL DEFAULT false;
ALTER TABLE org_auction_approvals ADD COLUMN on_behalf boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN procurement_approvals.on_behalf IS 'Signed by an owner for a role that had no active member.';
COMMENT ON COLUMN org_auction_approvals.on_behalf IS 'Signed by an owner for a role that had no active member.';

-- +goose Down
ALTER TABLE org_auction_approvals DROP COLUMN on_behalf;
ALTER TABLE procurement_approvals DROP COLUMN on_behalf;

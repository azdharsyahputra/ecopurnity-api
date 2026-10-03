-- +goose Up
-- Approval deadlock rule (src/domain/org.ts signingRoles): when a required role has no active member, the owner signs for
-- it. The row keeps that role (so the rule is satisfied) and decided_by is the owner; on_behalf marks it so the history
-- reads "Nama · Owner (atas nama Finance)".
ALTER TABLE procurement_approvals ADD COLUMN on_behalf boolean NOT NULL DEFAULT false;
ALTER TABLE org_auction_approvals ADD COLUMN on_behalf boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN procurement_approvals.on_behalf IS 'Signed by an owner for a role that had no active member.';
COMMENT ON COLUMN org_auction_approvals.on_behalf IS 'Signed by an owner for a role that had no active member.';

-- +goose Down
ALTER TABLE org_auction_approvals DROP COLUMN on_behalf;
ALTER TABLE procurement_approvals DROP COLUMN on_behalf;

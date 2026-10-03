-- +goose Up
-- Organization workspace (PRD §9): profile, roles and permissions, approval rules, inventory, procurement, collective
-- pools, business auctions with award and PO, supplier directory and relations, purchase history.
-- Paths: /orgs, /orgs/{orgId}/..., /mm/pools. Rules live in the frontend's src/domain/org.ts. See migrations/CONVENTIONS.md.
-- References: verification_requests (00002), markets, auctions (00003), trades, settlements, settlement_lines (00004).

-- ── Supplier directory (platform-wide) ───────────────────────────

CREATE TABLE suppliers (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name        text NOT NULL CHECK (length(btrim(name)) > 0),
  categories  category_id[] NOT NULL DEFAULT '{}',
  region      text NOT NULL,
  -- Directory seed rating; the API's `rating` blends it (weight 10 reviews) with buyer reviews and org ratings.
  seed_rating numeric(2,1) NOT NULL DEFAULT 0 CHECK (seed_rating BETWEEN 0 AND 5),
  verified    boolean NOT NULL DEFAULT false,              -- platform-verified business
  documents   text[] NOT NULL DEFAULT '{}',                -- legal document file names; `verify` needs >= 2 unless verified
  capacity    text NOT NULL DEFAULT '',                    -- free text, e.g. "600 ton kraft/bulan"
  -- Counterparty on trades (PO lines, pool sub-POs); created on first use like any party.
  party_id    uuid UNIQUE REFERENCES parties(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX suppliers_categories_idx ON suppliers USING gin (categories);
CREATE TRIGGER suppliers_updated_at BEFORE UPDATE ON suppliers FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE suppliers IS 'A supplier in the platform directory that every organization browses, rates and invites.';

CREATE TABLE supplier_scorecards (
  supplier_id uuid NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
  month       date NOT NULL CHECK (month = date_trunc('month', month)::date),  -- first day; API shows YYYY-MM
  price       numeric(5,2) NOT NULL CHECK (price BETWEEN 0 AND 100),
  reliability numeric(5,2) NOT NULL CHECK (reliability BETWEEN 0 AND 100),
  quality     numeric(5,2) NOT NULL CHECK (quality BETWEEN 0 AND 100),
  delivery    numeric(5,2) NOT NULL CHECK (delivery BETWEEN 0 AND 100),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (supplier_id, month)
);
COMMENT ON TABLE supplier_scorecards IS 'One month of a supplier''s 0-100 scorecard (price, reliability, quality, delivery).';

-- ── Profile, documents, settings ─────────────────────────────────
-- orgs.name is the profile name; everything else of OrgProfile lives here.

CREATE TABLE org_profiles (
  org_id       uuid PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
  org_type     text CHECK (org_type IN ('PT','CV','Koperasi','UMKM','Asosiasi','Kelompok tani','Yayasan')),  -- null for seeded orgs
  industry     text NOT NULL DEFAULT '',                   -- POST /orgs sets "<type> · <category label>"
  location     text NOT NULL DEFAULT '',                   -- address line ("Bandung, Jawa Barat")
  region       text,                                       -- public BusinessProfile.region; else last part of location
  description  text NOT NULL DEFAULT '',
  categories   category_id[] NOT NULL DEFAULT '{}',        -- ordered; analytics sorts categories by this order
  nib          text NOT NULL DEFAULT '' CHECK (nib = '' OR nib ~ '^[0-9]{13}$'),
  npwp         text NOT NULL DEFAULT '',                   -- POST /orgs takes 15/16 digits raw, PUT /profile the dotted format
  akta         text NOT NULL DEFAULT '',
  hours_days   smallint[] NOT NULL DEFAULT '{0,1,2,3,4}' CHECK (hours_days <@ '{0,1,2,3,4,5,6}'),
  hours_from   time NOT NULL DEFAULT '08:00',
  hours_to     time NOT NULL DEFAULT '17:00',
  website      text,                                       -- not in the API yet
  logo_url     text,                                       -- not in the API yet
  verification text NOT NULL DEFAULT 'unverified' CHECK (verification IN ('unverified','pending','verified','rejected')),
  verification_request_id uuid REFERENCES verification_requests(id),  -- latest request sent to the admin queue
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX org_profiles_verification_request_idx ON org_profiles (verification_request_id) WHERE verification_request_id IS NOT NULL;
CREATE TRIGGER org_profiles_updated_at BEFORE UPDATE ON org_profiles FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE org_profiles IS 'Business profile, legal data and verification state of an organization (1:1 orgs).';

CREATE TABLE org_documents (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id      uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  kind        text NOT NULL CHECK (kind IN ('nib','npwp','akta','other')),
  name        text NOT NULL CHECK (length(btrim(name)) > 0),  -- file name
  uploaded_by uuid REFERENCES users(id),
  uploaded_at timestamptz NOT NULL DEFAULT now()
);
-- A new nib/npwp/akta replaces the previous one (upsert on this index); `other` accumulates.
CREATE UNIQUE INDEX org_documents_kind_key ON org_documents (org_id, kind) WHERE kind <> 'other';
CREATE INDEX org_documents_org_idx ON org_documents (org_id, uploaded_at);
COMMENT ON TABLE org_documents IS 'A legal document an organization uploaded for business verification.';

CREATE TABLE org_settings (
  org_id             uuid PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
  departments        text[] NOT NULL DEFAULT '{Direksi,Pengadaan,Keuangan,Operasional,Penjualan}',
  savings_target_idr idr NOT NULL DEFAULT 10000000,       -- overview stats.savingsTargetIdr (monthly)
  service_regions    text[] NOT NULL DEFAULT '{}',        -- inventory logistics.regions
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER org_settings_updated_at BEFORE UPDATE ON org_settings FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE org_settings IS 'Team settings of an organization that are not roles or approval rules (1:1 orgs).';

-- ── Roles and permission matrix ──────────────────────────────────
-- Built-in roles (owner, procurement, finance, operations, sales) get a row per org at creation like custom ones, so
-- the matrix is editable per org and org_members.role has a real FK. Permissions are 'module.action' strings: the
-- matrix is always read and replaced whole per role, `can()` is one `= ANY` on the member's role row, and the CHECK
-- pins the value set (9 modules x 4 actions). Owner is hard-wired to everything in the application regardless.

CREATE TABLE org_roles (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id      uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  key         text NOT NULL CHECK (key IN ('owner','procurement','finance','operations','sales') OR key LIKE 'custom-%'),
  label       text NOT NULL CHECK (length(btrim(label)) > 0),
  custom      boolean GENERATED ALWAYS AS (key LIKE 'custom-%') STORED,
  position    smallint NOT NULL DEFAULT 0,                 -- order in OrgSettings.roles
  permissions text[] NOT NULL DEFAULT '{}' CHECK (
    array_to_string(permissions, ',') ~
    '^((procurement|auctions|collective|suppliers|inventory|transactions|analytics|team|profile)\.(view|create|approve|manage)(,|$))*$'),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, key)
);
CREATE TRIGGER org_roles_updated_at BEFORE UPDATE ON org_roles FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE org_roles IS 'A role of an organization (built-in or custom) with its permission matrix as module.action strings.';

-- Existing orgs get the built-in roles with the default matrix (src/domain/org.ts DEFAULT_PERMISSIONS) and any other
-- role their members already hold, so the FK below validates. New orgs: the application inserts the same rows.
INSERT INTO org_roles (org_id, key, label, position, permissions)
SELECT o.id, r.key, r.label, r.position, r.permissions
FROM orgs o CROSS JOIN (VALUES
  ('owner', 'Owner', 0, ARRAY(SELECT m || '.' || a
     FROM unnest('{procurement,auctions,collective,suppliers,inventory,transactions,analytics,team,profile}'::text[]) m,
          unnest('{view,create,approve,manage}'::text[]) a)),
  ('procurement', 'Procurement', 1, '{procurement.view,procurement.create,procurement.manage,auctions.view,auctions.create,auctions.manage,collective.view,collective.create,collective.manage,suppliers.view,suppliers.create,suppliers.manage,inventory.view,transactions.view,transactions.create,analytics.view,team.view,profile.view}'::text[]),
  ('finance', 'Finance', 2, '{procurement.view,procurement.approve,auctions.view,auctions.approve,collective.view,suppliers.view,inventory.view,transactions.view,transactions.approve,transactions.manage,analytics.view,team.view,profile.view}'::text[]),
  ('operations', 'Operations', 3, '{procurement.view,procurement.create,auctions.view,collective.view,suppliers.view,inventory.view,inventory.create,inventory.manage,transactions.view,transactions.manage,analytics.view,team.view,profile.view}'::text[]),
  ('sales', 'Sales', 4, '{procurement.view,auctions.view,collective.view,suppliers.view,inventory.view,transactions.view,analytics.view,team.view,profile.view}'::text[])
) AS r(key, label, position, permissions);
INSERT INTO org_roles (org_id, key, label, position)
SELECT DISTINCT m.org_id, m.role, m.role, 99 FROM org_members m
WHERE NOT EXISTS (SELECT 1 FROM org_roles r WHERE r.org_id = m.org_id AND r.key = m.role);

-- Deferred so an org, its roles and its first member can be inserted in any order within one transaction.
ALTER TABLE org_members ADD CONSTRAINT org_members_role_fkey
  FOREIGN KEY (org_id, role) REFERENCES org_roles (org_id, key) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE org_approval_rules (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id         uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  position       smallint NOT NULL DEFAULT 0,              -- rule order decides the order of required approvers
  label          text NOT NULL CHECK (length(btrim(label)) > 0),
  min_amount_idr idr NOT NULL,                             -- applies when the value is strictly above this
  approvers      text[] NOT NULL CHECK (cardinality(approvers) > 0),  -- role keys; not FK-checked (spec: not validated)
  applies_to     text[] NOT NULL CHECK (applies_to <@ '{procurement,auction}'),
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX org_approval_rules_org_idx ON org_approval_rules (org_id, position);
CREATE TRIGGER org_approval_rules_updated_at BEFORE UPDATE ON org_approval_rules FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE org_approval_rules IS 'An approval rule: above an amount, procurement and/or auctions need sign-off from these roles.';

-- ── Inventory ────────────────────────────────────────────────────

CREATE TABLE org_warehouses (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id      uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  name        text NOT NULL CHECK (length(btrim(name)) > 0),
  location    text NOT NULL DEFAULT '',
  capacity_m2 numeric(12,2) NOT NULL DEFAULT 0 CHECK (capacity_m2 >= 0),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, name)
);
CREATE TRIGGER org_warehouses_updated_at BEFORE UPDATE ON org_warehouses FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE org_warehouses IS 'A warehouse of an organization (also the fallback drop point for pool sub-POs).';

CREATE TABLE org_production_lines (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id       uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  line         text NOT NULL CHECK (length(btrim(line)) > 0),
  output_value qty NOT NULL,                               -- per month
  output_unit  text NOT NULL,
  utilization  numeric(4,3) NOT NULL DEFAULT 0 CHECK (utilization BETWEEN 0 AND 1),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX org_production_lines_org_idx ON org_production_lines (org_id);
CREATE TRIGGER org_production_lines_updated_at BEFORE UPDATE ON org_production_lines FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE org_production_lines IS 'A production line and its monthly output (inventory `capacity`).';

CREATE TABLE org_fleet (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id       uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  vehicle_type text NOT NULL CHECK (length(btrim(vehicle_type)) > 0),
  count        int NOT NULL CHECK (count >= 0),
  capacity     text NOT NULL DEFAULT '',                   -- free text, e.g. "5 ton"
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX org_fleet_org_idx ON org_fleet (org_id);
CREATE TRIGGER org_fleet_updated_at BEFORE UPDATE ON org_fleet FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE org_fleet IS 'A vehicle type in an organization''s fleet (inventory `logistics.fleet`).';

CREATE TABLE inventory_items (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id         uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  sku            text NOT NULL DEFAULT '',                 -- not unique: imports may leave it blank
  name           text NOT NULL CHECK (length(btrim(name)) > 0),
  category_id    category_id,                              -- optional on single add
  warehouse      text NOT NULL DEFAULT '',                 -- free text as sent; not FK to org_warehouses
  quantity       qty NOT NULL,
  unit           text NOT NULL,
  moq            qty NOT NULL DEFAULT 0,
  lead_time_days int NOT NULL DEFAULT 0 CHECK (lead_time_days >= 0),
  quality_spec   text NOT NULL DEFAULT '',
  created_by     uuid REFERENCES users(id),
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX inventory_items_org_idx ON inventory_items (org_id, created_at DESC);
CREATE TRIGGER inventory_items_updated_at BEFORE UPDATE ON inventory_items FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE inventory_items IS 'A stock item an organization holds.';

CREATE TABLE supply_schedules (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id       uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  item         text NOT NULL CHECK (length(btrim(item)) > 0),
  quantity     qty NOT NULL CHECK (quantity > 0),
  unit         text NOT NULL,
  every        text NOT NULL DEFAULT 'monthly' CHECK (every IN ('weekly','monthly')),
  counterparty text NOT NULL DEFAULT '',
  direction    text NOT NULL DEFAULT 'in' CHECK (direction IN ('in','out')),
  next_at      timestamptz NOT NULL DEFAULT now(),
  created_by   uuid REFERENCES users(id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX supply_schedules_org_idx ON supply_schedules (org_id, next_at);
CREATE TRIGGER supply_schedules_updated_at BEFORE UPDATE ON supply_schedules FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE supply_schedules IS 'A recurring inbound or outbound supply of an item with a counterparty.';

-- ── Collective pools (platform-wide, PRD §9.5 / F6) ──────────────
-- pool_markets is folded in: a pool forms at most one market and its round (market_id, auction_id, formed_by), so a
-- link table would only add a join.

CREATE TABLE collective_pools (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  title               text NOT NULL CHECK (length(btrim(title)) > 0),
  category_id         category_id NOT NULL,
  spec                text NOT NULL DEFAULT '',
  region              text NOT NULL DEFAULT '',
  deadline            timestamptz NOT NULL,
  unit                text NOT NULL,
  base_unit_price_idr idr NOT NULL CHECK (base_unit_price_idr > 0),
  ref_qty             qty NOT NULL CHECK (ref_qty > 0),        -- lot size the base price is quoted for
  threshold_qty       qty NOT NULL CHECK (threshold_qty > 0),  -- combined demand that justifies a market (8 x ref at creation)
  status              text NOT NULL DEFAULT 'open' CHECK (status IN ('open','market_requested','market_live','settled')),
  market_requested_at timestamptz,
  market_id           uuid REFERENCES markets(id),
  auction_id          uuid UNIQUE REFERENCES auctions(id),     -- the round the maker opened for the whole pool lot
  formed_by           uuid REFERENCES users(id),               -- market maker who formed the market
  formed_at           timestamptz,
  settlement_id       uuid UNIQUE REFERENCES settlements(id),  -- round settlement (00004): winner, price, at, by
  created_by          uuid REFERENCES users(id),               -- null for seeded pools
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CHECK (status <> 'market_requested' OR market_requested_at IS NOT NULL),
  CHECK (status NOT IN ('market_live','settled') OR (market_id IS NOT NULL AND auction_id IS NOT NULL AND formed_by IS NOT NULL)),
  CHECK (status <> 'settled' OR settlement_id IS NOT NULL)
);
CREATE INDEX collective_pools_created_idx ON collective_pools (created_at DESC);
CREATE INDEX collective_pools_requested_idx ON collective_pools (market_requested_at) WHERE status = 'market_requested';
CREATE INDEX collective_pools_maker_idx ON collective_pools (formed_by) WHERE formed_by IS NOT NULL;
CREATE INDEX collective_pools_open_match_idx ON collective_pools (category_id, unit) WHERE status = 'open';
CREATE INDEX collective_pools_market_idx ON collective_pools (market_id) WHERE market_id IS NOT NULL;
CREATE TRIGGER collective_pools_updated_at BEFORE UPDATE ON collective_pools FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE collective_pools IS 'A collective procurement pool: businesses add demand until a market maker forms a market for it.';

CREATE TABLE pool_members (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  pool_id            uuid NOT NULL REFERENCES collective_pools(id) ON DELETE CASCADE,
  org_id             uuid REFERENCES orgs(id) ON DELETE CASCADE,  -- platform org; null for an external business
  name               text,                                         -- external business name (org members show orgs.name)
  quantity           qty NOT NULL CHECK (quantity > 0),
  opt_in             boolean NOT NULL DEFAULT false,               -- show the name to other members, else "Bisnis lain #n"
  -- Delivery address of this member's sub-PO: request delivery location, else main warehouse, else profile location.
  drop_point         text,
  -- This member's line of the pool settlement (00004): settled quantity, share, amount and its sub-PO trade.
  settlement_line_id uuid UNIQUE REFERENCES settlement_lines(id),
  created_at         timestamptz NOT NULL DEFAULT now(),          -- join order; "#n" masking follows it
  updated_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (pool_id, org_id),
  CHECK (org_id IS NOT NULL OR btrim(coalesce(name, '')) <> '')
);
CREATE INDEX pool_members_pool_idx ON pool_members (pool_id, created_at);
CREATE INDEX pool_members_org_idx ON pool_members (org_id) WHERE org_id IS NOT NULL;
CREATE TRIGGER pool_members_updated_at BEFORE UPDATE ON pool_members FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE pool_members IS 'A business''s demand in a collective pool (one row per org; external businesses by name).';

-- ── Procurement requests (PRD §9.4) ──────────────────────────────

CREATE TABLE procurement_requests (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id             uuid NOT NULL REFERENCES orgs(id),
  code               text NOT NULL UNIQUE DEFAULT next_code('PRQ'),
  need               text NOT NULL CHECK (length(btrim(need)) > 0),
  category_id        category_id NOT NULL,
  quantity           qty NOT NULL CHECK (quantity > 0),
  unit               text NOT NULL,
  budget_idr         idr NOT NULL CHECK (budget_idr > 0),
  deadline           timestamptz NOT NULL,
  spec               text NOT NULL DEFAULT '',
  delivery_location  text NOT NULL DEFAULT '',
  visibility         text NOT NULL CHECK (visibility IN ('public','private','invite','aggregate')),
  status             text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','pending_approval','approved','published',
                       'in_auction','in_collective','awarded','po_issued','rejected','cancelled')),
  -- Snapshot from the approval rules at create/submit; later rule changes do not touch it. Role keys, not FK-checked.
  required_approvers text[] NOT NULL DEFAULT '{}',
  -- Pool the request joined (`collective`); kept after settlement (po_issued), cleared on leave. The request's
  -- auction is the org_auctions row pointing here that is not rejected (see org_auctions_procurement_key).
  pool_id            uuid REFERENCES collective_pools(id),
  created_by         uuid NOT NULL REFERENCES users(id),
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CHECK (status <> 'in_collective' OR pool_id IS NOT NULL)
);
CREATE INDEX procurement_requests_org_idx ON procurement_requests (org_id, updated_at DESC);
CREATE INDEX procurement_requests_pending_idx ON procurement_requests (org_id) WHERE status = 'pending_approval';
CREATE INDEX procurement_requests_pool_idx ON procurement_requests (pool_id) WHERE pool_id IS NOT NULL;
CREATE TRIGGER procurement_requests_updated_at BEFORE UPDATE ON procurement_requests FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE procurement_requests IS 'A procurement request (PRQ) of an organization moving through approval to auction, pool or PO.';

CREATE TABLE org_procurement_invites (
  procurement_request_id uuid NOT NULL REFERENCES procurement_requests(id) ON DELETE CASCADE,
  supplier_id            uuid NOT NULL REFERENCES suppliers(id),
  PRIMARY KEY (procurement_request_id, supplier_id)
);
CREATE INDEX org_procurement_invites_supplier_idx ON org_procurement_invites (supplier_id);
COMMENT ON TABLE org_procurement_invites IS 'A supplier invited to an invite-only procurement request (invitedSupplierIds).';

CREATE TABLE procurement_approvals (
  id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  procurement_request_id uuid NOT NULL REFERENCES procurement_requests(id) ON DELETE CASCADE,
  role                   text NOT NULL,                    -- role key the decision was made for
  decision               text NOT NULL CHECK (decision IN ('approved','rejected')),
  note                   text,
  decided_by             uuid NOT NULL REFERENCES users(id),
  decided_at             timestamptz NOT NULL DEFAULT now(),
  UNIQUE (procurement_request_id, role),
  CHECK (decision = 'approved' OR btrim(coalesce(note, '')) <> '')
);
COMMENT ON TABLE procurement_approvals IS 'One required role''s approve/reject decision on a procurement request.';

-- ── Business auctions (PRD §9.6) ─────────────────────────────────

CREATE TABLE org_auctions (
  id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id                 uuid NOT NULL REFERENCES orgs(id),
  code                   text NOT NULL UNIQUE DEFAULT next_code('OAU'),
  title                  text NOT NULL CHECK (length(btrim(title)) > 0),
  category_id            category_id NOT NULL,
  type                   text NOT NULL CHECK (type IN ('forward','reverse','sealed','dutch')),
  objective              text NOT NULL CHECK (objective IN ('procurement','selling')),
  -- rules (multiLot is derived from the lot count)
  min_step_idr           idr NOT NULL DEFAULT 0,
  bid_visibility         text NOT NULL CHECK (bid_visibility IN ('full','rank_only','sealed')),
  auto_extension         boolean NOT NULL DEFAULT true,
  withdraw_rule          text NOT NULL CHECK (withdraw_rule IN ('anytime','before_last_30','never')),
  award_rule             text NOT NULL CHECK (award_rule IN ('lowest','weighted','split','bundled')),
  weight_price           numeric(6,2) NOT NULL DEFAULT 60 CHECK (weight_price >= 0),  -- weights need not sum to 100
  weight_quality         numeric(6,2) NOT NULL DEFAULT 20 CHECK (weight_quality >= 0),
  weight_delivery        numeric(6,2) NOT NULL DEFAULT 10 CHECK (weight_delivery >= 0),
  weight_reliability     numeric(6,2) NOT NULL DEFAULT 10 CHECK (weight_reliability >= 0),
  -- qualification
  qual_documents         text[] NOT NULL DEFAULT '{}',
  qual_min_rating        numeric(2,1) NOT NULL DEFAULT 0 CHECK (qual_min_rating BETWEEN 0 AND 5),
  qual_regions           text[] NOT NULL DEFAULT '{}',
  -- schedule
  starts_at              timestamptz,                      -- null = as soon as it opens
  duration_minutes       int NOT NULL CHECK (duration_minutes > 0),
  procurement_request_id uuid REFERENCES procurement_requests(id),  -- set only when the request was linked (approved/published)
  -- Stored: pending_approval, rejected, and the status set when it opens; the API derives live/closed from the lots.
  status                 text NOT NULL CHECK (status IN ('pending_approval','scheduled','live','closed','awarded','rejected')),
  value_idr              idr NOT NULL,                     -- sum of lot quantity x reserve price
  required_approvers     text[] NOT NULL DEFAULT '{}',     -- snapshot from approval rules at creation
  created_by             uuid NOT NULL REFERENCES users(id),
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX org_auctions_org_idx ON org_auctions (org_id, created_at DESC);
CREATE INDEX org_auctions_pending_idx ON org_auctions (org_id) WHERE status = 'pending_approval';
-- A request runs in at most one auction at a time; a rejected auction hands it back.
CREATE UNIQUE INDEX org_auctions_procurement_key ON org_auctions (procurement_request_id)
  WHERE procurement_request_id IS NOT NULL AND status <> 'rejected';
CREATE TRIGGER org_auctions_updated_at BEFORE UPDATE ON org_auctions FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE org_auctions IS 'A business auction (OAU) of an organization: one or more lots, each run as an economy auction.';

CREATE TABLE org_auction_lots (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_auction_id    uuid NOT NULL REFERENCES org_auctions(id) ON DELETE CASCADE,
  position          smallint NOT NULL CHECK (position > 0),  -- API lot id is 'lot-' || position
  item              text NOT NULL CHECK (length(btrim(item)) > 0),
  quantity          qty NOT NULL CHECK (quantity > 0),
  unit              text NOT NULL,
  spec              text NOT NULL DEFAULT '',
  reserve_price_idr idr NOT NULL CHECK (reserve_price_idr > 0),  -- per unit; also the opening price
  auction_id        uuid UNIQUE REFERENCES auctions(id),         -- economy auction once the lot is live
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_auction_id, position)
);
CREATE TRIGGER org_auction_lots_updated_at BEFORE UPDATE ON org_auction_lots FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE org_auction_lots IS 'A lot of a business auction.';

CREATE TABLE org_auction_invites (
  org_auction_id uuid NOT NULL REFERENCES org_auctions(id) ON DELETE CASCADE,
  supplier_id    uuid NOT NULL REFERENCES suppliers(id),
  PRIMARY KEY (org_auction_id, supplier_id)
);
CREATE INDEX org_auction_invites_supplier_idx ON org_auction_invites (supplier_id);
COMMENT ON TABLE org_auction_invites IS 'A supplier invited to a business auction (`invited`).';

CREATE TABLE org_auction_approvals (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_auction_id uuid NOT NULL REFERENCES org_auctions(id) ON DELETE CASCADE,
  role           text NOT NULL,
  decision       text NOT NULL CHECK (decision IN ('approved','rejected')),
  note           text,
  decided_by     uuid NOT NULL REFERENCES users(id),
  decided_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_auction_id, role),
  CHECK (decision = 'approved' OR btrim(coalesce(note, '')) <> '')
);
COMMENT ON TABLE org_auction_approvals IS 'One required role''s approve/reject decision on a business auction.';

-- Evaluation snapshot: one row per bidder per lot (their best price), written once the lot closes, mapped onto the
-- supplier directory for scorecards. Award lines point at these, so allocations always name a real offer.
CREATE TABLE org_auction_offers (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_auction_lot_id uuid NOT NULL REFERENCES org_auction_lots(id) ON DELETE CASCADE,
  party_id           uuid NOT NULL REFERENCES parties(id),     -- the bidder
  supplier_id        uuid REFERENCES suppliers(id),            -- directory match; null when the bidder is not listed
  price_idr          idr NOT NULL,                             -- best per-unit price (lowest buying, highest selling)
  capacity           qty NOT NULL,                             -- most this bidder can deliver, in the lot unit
  submitted_at       timestamptz NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_auction_lot_id, party_id)
);
COMMENT ON TABLE org_auction_offers IS 'A bidder''s best offer on a closed business auction lot, as evaluated for award.';

CREATE TABLE org_awards (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_auction_id uuid NOT NULL UNIQUE REFERENCES org_auctions(id) ON DELETE CASCADE,
  reason         text NOT NULL CHECK (length(btrim(reason)) > 0),
  awarded_by     uuid NOT NULL REFERENCES users(id),
  awarded_at     timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE org_awards IS 'The award decision of a business auction (one per auction).';

CREATE TABLE purchase_orders (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id       uuid NOT NULL REFERENCES orgs(id),
  org_award_id uuid NOT NULL UNIQUE REFERENCES org_awards(id),
  po_number    text NOT NULL,                              -- PO-{initials}-{4 digits}; retry on conflict
  issued_by    uuid NOT NULL REFERENCES users(id),
  issued_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, po_number)
);
COMMENT ON TABLE purchase_orders IS 'The PO issued for an award; each award line becomes one trade.';

CREATE TABLE org_award_lines (
  id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_award_id         uuid NOT NULL REFERENCES org_awards(id) ON DELETE CASCADE,
  org_auction_lot_id   uuid NOT NULL REFERENCES org_auction_lots(id),
  position             smallint NOT NULL DEFAULT 0,        -- order within the lot
  org_auction_offer_id uuid NOT NULL REFERENCES org_auction_offers(id),  -- AllocationLine.offerId
  quantity             qty NOT NULL CHECK (quantity > 0),
  price_idr            idr NOT NULL,                       -- per unit
  trade_id             uuid UNIQUE REFERENCES trades(id),  -- set when the PO is issued
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_award_id, org_auction_lot_id, position)
);
CREATE INDEX org_award_lines_offer_idx ON org_award_lines (org_auction_offer_id);
CREATE TRIGGER org_award_lines_updated_at BEFORE UPDATE ON org_award_lines FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE org_award_lines IS 'One allocation of an award: a quantity of a lot to one offer, and the trade it became.';

-- ── Supplier relations and purchase history (PRD §9.7, §9.9) ─────

CREATE TABLE org_suppliers (
  org_id       uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  supplier_id  uuid NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
  relation     text NOT NULL DEFAULT 'none' CHECK (relation IN ('none','shortlisted','invited','verified','blocked')),
  my_rating    smallint CHECK (my_rating BETWEEN 1 AND 5),
  block_reason text,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, supplier_id),
  CHECK (relation <> 'blocked' OR btrim(coalesce(block_reason, '')) <> '')
);
CREATE INDEX org_suppliers_rating_idx ON org_suppliers (supplier_id) WHERE my_rating IS NOT NULL;
CREATE TRIGGER org_suppliers_updated_at BEFORE UPDATE ON org_suppliers FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE org_suppliers IS 'An organization''s relation to a directory supplier and its own 1-5 rating.';

-- Purchases made before or outside the platform's trades, for analytics. Awarded auctions are read from awards.
CREATE TABLE org_purchase_history (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id          uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  code            text NOT NULL,
  month           date NOT NULL CHECK (month = date_trunc('month', month)::date),
  item            text NOT NULL,
  category_id     category_id NOT NULL,
  supplier_id     uuid NOT NULL REFERENCES suppliers(id),
  quantity        qty NOT NULL CHECK (quantity > 0),
  unit            text NOT NULL,
  unit_price_idr  idr NOT NULL,                            -- paid
  budget_unit_idr idr NOT NULL,
  market_unit_idr idr NOT NULL,
  via             text NOT NULL CHECK (via IN ('auction','collective','direct')),
  bidders         int CHECK (bidders >= 0),                -- auction rows only
  opening_idr     idr,                                     -- auction rows only
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, code)
);
CREATE INDEX org_purchase_history_month_idx ON org_purchase_history (org_id, month);
CREATE INDEX org_purchase_history_supplier_idx ON org_purchase_history (org_id, supplier_id, month DESC);
COMMENT ON TABLE org_purchase_history IS 'A past purchase of an organization (month, item, supplier, price vs budget and market).';

-- +goose Down
DROP TABLE org_purchase_history;
DROP TABLE org_suppliers;
DROP TABLE org_award_lines;
DROP TABLE purchase_orders;
DROP TABLE org_awards;
DROP TABLE org_auction_offers;
DROP TABLE org_auction_approvals;
DROP TABLE org_auction_invites;
DROP TABLE org_auction_lots;
DROP TABLE org_auctions;
DROP TABLE procurement_approvals;
DROP TABLE org_procurement_invites;
DROP TABLE procurement_requests;
DROP TABLE pool_members;
DROP TABLE collective_pools;
DROP TABLE supply_schedules;
DROP TABLE inventory_items;
DROP TABLE org_fleet;
DROP TABLE org_production_lines;
DROP TABLE org_warehouses;
DROP TABLE org_approval_rules;
ALTER TABLE org_members DROP CONSTRAINT org_members_role_fkey;
DROP TABLE org_roles;
DROP TABLE org_settings;
DROP TABLE org_documents;
DROP TABLE org_profiles;
DROP TABLE supplier_scorecards;
DROP TABLE suppliers;

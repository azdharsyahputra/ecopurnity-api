-- +goose Up

CREATE TABLE opportunities (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code                text NOT NULL UNIQUE DEFAULT next_code('OPP'),
  title               text NOT NULL CHECK (length(btrim(title)) > 0),
  kind                text NOT NULL CHECK (kind IN ('collective_demand','supply_gap','market_gap','capacity_match')),
  category_id         category_id NOT NULL,
  region              text NOT NULL,
  status              text NOT NULL DEFAULT 'detected'
                      CHECK (status IN ('detected','forming','market_live','dismissed','closed')),
  unit                text NOT NULL,
  demand_value        qty NOT NULL,
  supply_value        qty NOT NULL,
  participant_count   int NOT NULL DEFAULT 0 CHECK (participant_count >= 0),
  potential_value_idr idr NOT NULL,
  suggested_mechanism text NOT NULL CHECK (suggested_mechanism IN ('forward_auction','reverse_auction','sealed_bid','dutch_auction',
                                                                   'direct_market','collective_procurement')),
  confidence          numeric(4,3) NOT NULL CHECK (confidence BETWEEN 0 AND 1),
  mechanism_reason    text NOT NULL,
  description         text NOT NULL,
  required_contribution text NOT NULL,
  detected_at         timestamptz NOT NULL DEFAULT now(),
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX opportunities_value_idx ON opportunities (potential_value_idr DESC);
CREATE INDEX opportunities_category_idx ON opportunities (category_id, status);
CREATE TRIGGER opportunities_updated_at BEFORE UPDATE ON opportunities FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE opportunities IS 'An economic opportunity detected by the engine (demand/supply gap in a category and region).';

CREATE TABLE opportunity_follows (
  user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  opportunity_id  uuid NOT NULL REFERENCES opportunities(id) ON DELETE CASCADE,
  created_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, opportunity_id)
);
CREATE INDEX opportunity_follows_opportunity_idx ON opportunity_follows (opportunity_id);
COMMENT ON TABLE opportunity_follows IS 'A user following an opportunity (relation `following`; a participant row means `joined`).';

CREATE TABLE markets (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code            text NOT NULL UNIQUE DEFAULT next_code('MKT'),
  name            text NOT NULL CHECK (length(btrim(name)) > 0),
  category_id     category_id NOT NULL,
  region          text NOT NULL,
  objective       text NOT NULL CHECK (objective IN ('procurement','selling','resource_exchange','service_exchange')),
  mechanism       text NOT NULL CHECK (mechanism IN ('forward_auction','reverse_auction','sealed_bid','dutch_auction',
                                                     'direct_market','collective_procurement')),
  status          text NOT NULL DEFAULT 'active' CHECK (status IN ('draft','formation','active','paused','closed','suspended')),
  maker_party_id  uuid NOT NULL REFERENCES parties(id),
  opportunity_id  uuid REFERENCES opportunities(id),
  description     text NOT NULL DEFAULT '',
  unit            text NOT NULL,
  demand_value    qty NOT NULL,
  supply_value    qty NOT NULL,
  price_min_idr   idr NOT NULL,
  price_max_idr   idr NOT NULL,
  volume_30d_idr  idr NOT NULL DEFAULT 0,
  reviewed_at     timestamptz,
  created_by      uuid REFERENCES users(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CHECK (price_min_idr <= price_max_idr)
);
CREATE INDEX markets_volume_idx ON markets (volume_30d_idr DESC);
CREATE INDEX markets_maker_idx ON markets (maker_party_id);
CREATE INDEX markets_opportunity_idx ON markets (opportunity_id) WHERE opportunity_id IS NOT NULL;
CREATE TRIGGER markets_updated_at BEFORE UPDATE ON markets FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE markets IS 'A market run by a market maker: category, region, mechanism and lifecycle status.';

ALTER TABLE audit_log ADD CONSTRAINT audit_log_market_id_fkey FOREIGN KEY (market_id) REFERENCES markets(id);

CREATE TABLE market_operators (
  market_id   uuid NOT NULL REFERENCES markets(id) ON DELETE CASCADE,
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  added_by    uuid REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (market_id, user_id)
);
CREATE INDEX market_operators_user_idx ON market_operators (user_id);
COMMENT ON TABLE market_operators IS 'A market maker user who operates a market (MM workspace access).';

CREATE TABLE market_settings (
  market_id             uuid PRIMARY KEY REFERENCES markets(id) ON DELETE CASCADE,
  approval              text NOT NULL DEFAULT 'manual' CHECK (approval IN ('auto','manual')),
  supplier_verification text NOT NULL DEFAULT 'documents' CHECK (supplier_verification IN ('none','documents','verified_business')),
  updated_by            uuid REFERENCES users(id),
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER market_settings_updated_at BEFORE UPDATE ON market_settings FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE market_settings IS 'Participant approval mode and supplier verification requirement of a market (1:1).';

CREATE TABLE market_rule_versions (
  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  market_id             uuid NOT NULL REFERENCES markets(id) ON DELETE CASCADE,
  version               int NOT NULL CHECK (version >= 1),
  rules                 jsonb NOT NULL CHECK (jsonb_typeof(rules) = 'object'),
  effective_from_round  int NOT NULL CHECK (effective_from_round >= 1),
  author                text NOT NULL,
  created_by            uuid REFERENCES users(id),
  reason                text,
  created_at            timestamptz NOT NULL DEFAULT now(),
  UNIQUE (market_id, version)
);
COMMENT ON TABLE market_rule_versions IS 'One immutable version of a market''s rules and the first round it governs.';

CREATE TABLE market_participants (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  market_id   uuid NOT NULL REFERENCES markets(id) ON DELETE CASCADE,
  party_id    uuid NOT NULL REFERENCES parties(id),
  role        text NOT NULL CHECK (role IN ('buyer','supplier')),
  status      text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','active','rejected','suspended')),
  verified    boolean NOT NULL DEFAULT false,
  note        text,
  joined_at   timestamptz NOT NULL DEFAULT now(),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (market_id, party_id)
);
CREATE INDEX market_participants_market_idx ON market_participants (market_id, status, role);
CREATE INDEX market_participants_party_idx ON market_participants (party_id);
CREATE TRIGGER market_participants_updated_at BEFORE UPDATE ON market_participants FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE market_participants IS 'A buyer or supplier in a market maker''s participant list, with approval status.';

CREATE TABLE market_flags (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  market_id   uuid NOT NULL REFERENCES markets(id) ON DELETE CASCADE,
  label       text NOT NULL CHECK (length(btrim(label)) > 0),
  flagged_by  uuid REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX market_flags_market_idx ON market_flags (market_id, created_at DESC);
COMMENT ON TABLE market_flags IS 'A moderation flag on a market raised by an admin or the rule engine (MarketFlag).';

CREATE TABLE market_reports (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  market_id         uuid NOT NULL REFERENCES markets(id) ON DELETE CASCADE,
  reporter_party_id uuid NOT NULL REFERENCES parties(id),
  reason            text NOT NULL CHECK (length(btrim(reason)) > 0),
  context           text,
  created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX market_reports_market_idx ON market_reports (market_id, created_at DESC);
COMMENT ON TABLE market_reports IS 'A participant''s report about a market, shown to admins (AdminMarket.reports).';

CREATE TABLE market_disputes (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  market_id     uuid NOT NULL REFERENCES markets(id) ON DELETE CASCADE,
  title         text NOT NULL CHECK (length(btrim(title)) > 0),
  parties       text NOT NULL,
  status        text NOT NULL DEFAULT 'open' CHECK (status IN ('open','evidence','review','resolved')),
  resolution    text,
  escalated_to  uuid,
  escalated_by  uuid REFERENCES users(id),
  escalated_at  timestamptz,
  opened_at     timestamptz NOT NULL DEFAULT now(),
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CHECK ((escalated_to IS NULL) = (escalated_at IS NULL))
);
CREATE INDEX market_disputes_market_idx ON market_disputes (market_id, opened_at DESC);
CREATE UNIQUE INDEX market_disputes_escalated_key ON market_disputes (escalated_to) WHERE escalated_to IS NOT NULL;
CREATE TRIGGER market_disputes_updated_at BEFORE UPDATE ON market_disputes FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE market_disputes IS 'A dispute handled by the market maker inside one market, optionally escalated to admin.';

CREATE TABLE mm_pipeline (
  opportunity_id  uuid PRIMARY KEY REFERENCES opportunities(id) ON DELETE CASCADE,
  stage           text NOT NULL CHECK (stage IN ('detected','evaluating','forming','market_live','dismissed')),
  dismiss_reason  text,
  market_id       uuid REFERENCES markets(id),
  updated_by      uuid REFERENCES users(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CHECK (stage <> 'dismissed' OR length(btrim(dismiss_reason)) > 0),
  CHECK (stage <> 'market_live' OR market_id IS NOT NULL)
);
CREATE TRIGGER mm_pipeline_updated_at BEFORE UPDATE ON mm_pipeline FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE mm_pipeline IS 'The market maker pipeline stage of an opportunity (PipelineCard.stage).';

CREATE TABLE watchlist (
  user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  market_id       uuid NOT NULL REFERENCES markets(id) ON DELETE CASCADE,
  joined_at       timestamptz,
  watch_price_idr idr CHECK (watch_price_idr > 0),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, market_id)
);
CREATE INDEX watchlist_market_idx ON watchlist (market_id);
CREATE TRIGGER watchlist_updated_at BEFORE UPDATE ON watchlist FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE watchlist IS 'A user''s personal state for one market: joined flag and median-price alert.';

CREATE TABLE listings (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code            text NOT NULL UNIQUE,
  kind            text NOT NULL CHECK (kind IN ('supply','demand')),
  status          text NOT NULL,
  owner_party_id  uuid NOT NULL REFERENCES parties(id),
  item            text NOT NULL CHECK (length(btrim(item)) > 0),
  category_id     category_id NOT NULL,
  quantity        qty NOT NULL,
  unit            text NOT NULL,
  location        text NOT NULL,
  spec            text NOT NULL DEFAULT '',
  delivery        text NOT NULL CHECK (delivery IN ('pickup','deliver','both')),
  market_id       uuid REFERENCES markets(id),
  price_idr       idr,
  available_from  timestamptz,
  expires_at      timestamptz,
  budget_idr      idr,
  deadline        timestamptz,
  auction_id      uuid,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CHECK (code LIKE CASE kind WHEN 'supply' THEN 'SUP-%' ELSE 'DEM-%' END),
  CHECK (
    (kind = 'supply' AND status IN ('available','reserved','in_market','sold','expired')
      AND price_idr IS NOT NULL AND available_from IS NOT NULL
      AND budget_idr IS NULL AND deadline IS NULL AND auction_id IS NULL) OR
    (kind = 'demand' AND status IN ('open','matched','in_market','fulfilled','expired','cancelled')
      AND budget_idr IS NOT NULL AND deadline IS NOT NULL
      AND price_idr IS NULL AND available_from IS NULL AND expires_at IS NULL)
  ),
  CHECK (quantity > 0 OR status = 'sold'),
  CHECK (status <> 'in_market' OR market_id IS NOT NULL OR auction_id IS NOT NULL)
);
CREATE INDEX listings_owner_idx ON listings (owner_party_id, created_at DESC);
CREATE INDEX listings_catalog_idx ON listings (created_at DESC) WHERE status IN ('available','in_market','open','matched');
CREATE INDEX listings_market_idx ON listings (market_id, kind, status) WHERE market_id IS NOT NULL;
CREATE INDEX listings_price_sample_idx ON listings (category_id, lower(unit));
CREATE TRIGGER listings_updated_at BEFORE UPDATE ON listings FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE listings IS 'A supply offer or a demand request posted by a participant (kind supply|demand).';

CREATE TABLE listing_events (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  listing_id  uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  status      text NOT NULL,
  note        text NOT NULL,
  at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX listing_events_listing_idx ON listing_events (listing_id, at DESC);
COMMENT ON TABLE listing_events IS 'One entry in a listing''s history (ListingDetail.history).';

CREATE TABLE listing_attachments (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  listing_id  uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  file_name   text NOT NULL CHECK (length(btrim(file_name)) > 0),
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX listing_attachments_listing_idx ON listing_attachments (listing_id, created_at);
COMMENT ON TABLE listing_attachments IS 'A file attached to a listing (Listing.attachments).';

CREATE TABLE opportunity_participants (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  opportunity_id  uuid NOT NULL REFERENCES opportunities(id) ON DELETE CASCADE,
  party_id        uuid NOT NULL REFERENCES parties(id),
  role            text NOT NULL CHECK (role IN ('buyer','supplier')),
  listing_id      uuid REFERENCES listings(id) ON DELETE SET NULL,
  quantity        qty CHECK (quantity > 0),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (opportunity_id, party_id)
);
CREATE INDEX opportunity_participants_party_idx ON opportunity_participants (party_id);
CREATE INDEX opportunity_participants_listing_idx ON opportunity_participants (listing_id) WHERE listing_id IS NOT NULL;
CREATE TRIGGER opportunity_participants_updated_at BEFORE UPDATE ON opportunity_participants FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE opportunity_participants IS 'A party taking part in an opportunity, with its contributed listing and quantity.';

CREATE TABLE auctions (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code                text NOT NULL UNIQUE DEFAULT next_code('AUC'),
  title               text NOT NULL CHECK (length(btrim(title)) > 0),
  market_id           uuid REFERENCES markets(id),
  round_no            int CHECK (round_no >= 1),
  rule_version_id     uuid REFERENCES market_rule_versions(id),
  owner_user_id       uuid REFERENCES users(id),
  owner_org_id        uuid REFERENCES orgs(id),
  category_id         category_id NOT NULL,
  type                text NOT NULL CHECK (type IN ('forward','reverse','sealed','dutch')),
  status              text NOT NULL DEFAULT 'scheduled'
                      CHECK (status IN ('scheduled','qualification','live','extended','closed','awarded','cancelled','frozen')),
  frozen_from         text CHECK (frozen_from IN ('scheduled','qualification','live','extended')),
  visibility          text NOT NULL CHECK (visibility IN ('full','rank_only','sealed')),
  lot_item            text NOT NULL,
  lot_spec            text NOT NULL DEFAULT '',
  quantity            qty NOT NULL CHECK (quantity > 0),
  unit                text NOT NULL,
  opening_price_idr   idr NOT NULL CHECK (opening_price_idr > 0),
  current_price_idr   idr,
  min_step_idr        idr NOT NULL DEFAULT 0,
  starts_at           timestamptz NOT NULL,
  ends_at             timestamptz NOT NULL,
  ext_window_minutes  int NOT NULL DEFAULT 2 CHECK (ext_window_minutes >= 0),
  ext_minutes         int NOT NULL DEFAULT 5 CHECK (ext_minutes >= 0),
  extension_count     int NOT NULL DEFAULT 0 CHECK (extension_count BETWEEN 0 AND 3),
  bid_count           int NOT NULL DEFAULT 0 CHECK (bid_count >= 0),
  participant_count   int NOT NULL DEFAULT 0 CHECK (participant_count >= 0),
  last_seq            bigint NOT NULL DEFAULT 0,
  rules               jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(rules) = 'array'),
  invitees            text[] NOT NULL DEFAULT '{}',
  clearing_price_idr  idr,
  median_price_idr    idr,
  created_by          uuid REFERENCES users(id),
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CHECK (ends_at > starts_at),
  CHECK (round_no IS NULL OR market_id IS NOT NULL),
  CHECK (rule_version_id IS NULL OR round_no IS NOT NULL),
  CHECK ((status = 'frozen') = (frozen_from IS NOT NULL)),
  CHECK (type <> 'sealed' OR visibility = 'sealed'),
  UNIQUE (market_id, round_no)
);
CREATE INDEX auctions_market_idx ON auctions (market_id, status) WHERE market_id IS NOT NULL;
CREATE INDEX auctions_owner_idx ON auctions (owner_user_id, created_at DESC) WHERE owner_user_id IS NOT NULL;
CREATE INDEX auctions_list_idx ON auctions (status, ends_at);
CREATE INDEX auctions_closing_idx ON auctions (ends_at) WHERE status IN ('live','extended');
CREATE INDEX auctions_starting_idx ON auctions (starts_at) WHERE status = 'scheduled';
CREATE TRIGGER auctions_updated_at BEFORE UPDATE ON auctions FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE auctions IS 'An auction: a market round, a personal buyer auction or an organization lot.';

ALTER TABLE listings ADD CONSTRAINT listings_auction_id_fkey FOREIGN KEY (auction_id) REFERENCES auctions(id);
CREATE UNIQUE INDEX listings_auction_key ON listings (auction_id) WHERE auction_id IS NOT NULL;

CREATE TABLE auction_qualifications (
  auction_id        uuid NOT NULL REFERENCES auctions(id) ON DELETE CASCADE,
  user_id           uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  status            text NOT NULL DEFAULT 'pending' CHECK (status IN ('not_started','pending','qualified','rejected')),
  document_name     text,
  rules_accepted_at timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (auction_id, user_id),
  CHECK (status <> 'qualified' OR (document_name IS NOT NULL AND rules_accepted_at IS NOT NULL))
);
CREATE INDEX auction_qualifications_user_idx ON auction_qualifications (user_id);
CREATE TRIGGER auction_qualifications_updated_at BEFORE UPDATE ON auction_qualifications FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE auction_qualifications IS 'A user''s qualification to bid in one auction.';

CREATE TABLE bids (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  auction_id       uuid NOT NULL REFERENCES auctions(id) ON DELETE CASCADE,
  seq              bigint NOT NULL,
  bidder_party_id  uuid NOT NULL REFERENCES parties(id),
  bidder_user_id   uuid REFERENCES users(id),
  bidder_no        int NOT NULL,
  price_idr        idr NOT NULL CHECK (price_idr > 0),
  status           text NOT NULL CHECK (status IN ('draft','submitted','leading','outbid','won','lost','withdrawn')),
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (auction_id, seq)
);
CREATE INDEX bids_best_idx ON bids (auction_id, price_idr, seq) WHERE status <> 'withdrawn';
CREATE INDEX bids_bidder_idx ON bids (bidder_party_id, auction_id, seq DESC);
CREATE TRIGGER bids_updated_at BEFORE UPDATE ON bids FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE bids IS 'One bid placed in an auction (append-only feed; status changes as others bid and on close).';

-- +goose StatementBegin
CREATE FUNCTION bids_before_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE auctions SET last_seq = last_seq + 1, bid_count = bid_count + 1
   WHERE id = NEW.auction_id RETURNING last_seq INTO NEW.seq;
  SELECT bidder_no INTO NEW.bidder_no FROM bids
   WHERE bidder_party_id = NEW.bidder_party_id AND auction_id = NEW.auction_id LIMIT 1;
  IF NEW.bidder_no IS NULL THEN
    UPDATE auctions SET participant_count = participant_count + 1
     WHERE id = NEW.auction_id RETURNING participant_count INTO NEW.bidder_no;
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER bids_assign_seq BEFORE INSERT ON bids FOR EACH ROW EXECUTE FUNCTION bids_before_insert();

CREATE TABLE auction_awards (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  auction_id  uuid NOT NULL UNIQUE REFERENCES auctions(id),
  awarded_by  uuid NOT NULL REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE auction_awards IS 'The buyer''s award decision on a closed auction.';

CREATE TABLE auction_award_lines (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  award_id    uuid NOT NULL REFERENCES auction_awards(id) ON DELETE CASCADE,
  bid_id      uuid NOT NULL REFERENCES bids(id),
  quantity    qty NOT NULL CHECK (quantity > 0),
  price_idr   idr NOT NULL CHECK (price_idr > 0),
  UNIQUE (award_id, bid_id)
);
COMMENT ON TABLE auction_award_lines IS 'One allocation line of an award: quantity given to a bidder at a price.';

CREATE TABLE matches (
  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id               uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  opportunity_id        uuid NOT NULL REFERENCES opportunities(id) ON DELETE CASCADE,
  have_listing_id       uuid REFERENCES listings(id) ON DELETE CASCADE,
  have_capacity_item_id uuid REFERENCES capacity_items(id) ON DELETE CASCADE,
  state                 text NOT NULL DEFAULT 'new' CHECK (state IN ('new','saved','connected','dismissed')),
  reason                text,
  conversation_id       uuid,
  score                 numeric(5,2) NOT NULL CHECK (score BETWEEN 0 AND 100),
  part_category         numeric(5,2) NOT NULL,
  part_distance         numeric(5,2) NOT NULL,
  part_coverage         numeric(5,2) NOT NULL,
  part_confidence       numeric(5,2) NOT NULL,
  distance_km           numeric(8,1) NOT NULL CHECK (distance_km >= 0),
  estimated_value_idr   idr NOT NULL,
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CHECK (num_nonnulls(have_listing_id, have_capacity_item_id) = 1),
  UNIQUE NULLS NOT DISTINCT (user_id, opportunity_id, have_listing_id, have_capacity_item_id)
);
CREATE TRIGGER matches_updated_at BEFORE UPDATE ON matches FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE matches IS 'A user''s smart-match state for one (have, opportunity) pair, with its score snapshot.';

-- +goose Down
DROP TABLE matches;
DROP TABLE auction_award_lines;
DROP TABLE auction_awards;
DROP TRIGGER bids_assign_seq ON bids;
DROP FUNCTION bids_before_insert();
DROP TABLE bids;
DROP TABLE auction_qualifications;
ALTER TABLE listings DROP CONSTRAINT listings_auction_id_fkey;
DROP TABLE auctions;
DROP TABLE opportunity_participants;
DROP TABLE listing_attachments;
DROP TABLE listing_events;
DROP TABLE listings;
DROP TABLE watchlist;
DROP TABLE mm_pipeline;
DROP TABLE market_disputes;
DROP TABLE market_reports;
DROP TABLE market_flags;
DROP TABLE market_participants;
DROP TABLE market_rule_versions;
DROP TABLE market_settings;
DROP TABLE market_operators;
ALTER TABLE audit_log DROP CONSTRAINT audit_log_market_id_fkey;
DROP TABLE markets;
DROP TABLE opportunity_follows;
DROP TABLE opportunities;

-- +goose Up

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;

-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE SEQUENCE public_code_seq START 46656;
-- +goose StatementBegin
CREATE FUNCTION next_code(prefix text) RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  n bigint := nextval('public_code_seq');
  chars text := '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ';
  out text := '';
BEGIN
  WHILE n > 0 LOOP
    out := substr(chars, (n % 36)::int + 1, 1) || out;
    n := n / 36;
  END LOOP;
  RETURN prefix || '-' || out;
END $$;
-- +goose StatementEnd

CREATE DOMAIN idr AS bigint CHECK (VALUE >= 0);
CREATE DOMAIN qty AS numeric(18,3) CHECK (VALUE >= 0);
CREATE DOMAIN category_id AS text
  CHECK (VALUE IN ('agri','food','packaging','manufacturing','logistics','it','energy'));

CREATE TABLE users (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name            text NOT NULL CHECK (length(btrim(name)) > 0),
  username        citext NOT NULL UNIQUE CHECK (username ~ '^[a-z0-9._-]{3,32}$'),
  email           citext NOT NULL UNIQUE,
  email_verified_at timestamptz,
  password_hash   text,
  google_sub      text UNIQUE,
  location        text,
  avatar_url      text,
  onboarded_at    timestamptz,
  status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','restricted','suspended')),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CHECK (password_hash IS NOT NULL OR google_sub IS NOT NULL)
);
CREATE TRIGGER users_updated_at BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE user_capabilities (
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  capability  text NOT NULL CHECK (capability IN ('market_maker','admin')),
  granted_at  timestamptz NOT NULL DEFAULT now(),
  granted_by  uuid REFERENCES users(id),
  PRIMARY KEY (user_id, capability)
);

CREATE TABLE sessions (
  token_hash   bytea PRIMARY KEY,
  user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  user_agent   text,
  ip           inet
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

CREATE TABLE auth_tokens (
  token_hash  bytea PRIMARY KEY,
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  purpose     text NOT NULL CHECK (purpose IN ('verify_email','reset_password')),
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX auth_tokens_user_idx ON auth_tokens (user_id, purpose);

CREATE TABLE orgs (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name        text NOT NULL CHECK (length(btrim(name)) > 0),
  slug        citext NOT NULL UNIQUE,
  created_by  uuid REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX orgs_name_key ON orgs (lower(name));
CREATE TRIGGER orgs_updated_at BEFORE UPDATE ON orgs FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE org_members (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id      uuid NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  user_id     uuid REFERENCES users(id) ON DELETE CASCADE,
  email       citext NOT NULL,
  name        text NOT NULL,
  role        text NOT NULL,
  department  text,
  status      text NOT NULL DEFAULT 'invited' CHECK (status IN ('invited','active')),
  invited_by  uuid REFERENCES users(id),
  joined_at   timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (org_id, email),
  CHECK (status = 'invited' OR user_id IS NOT NULL)
);
CREATE UNIQUE INDEX org_members_user_key ON org_members (org_id, user_id) WHERE user_id IS NOT NULL;
CREATE INDEX org_members_user_idx ON org_members (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX org_members_email_invited_idx ON org_members (email) WHERE status = 'invited';

CREATE TABLE parties (
  id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind      text NOT NULL CHECK (kind IN ('user','org','external')),
  user_id   uuid UNIQUE REFERENCES users(id),
  org_id    uuid UNIQUE REFERENCES orgs(id),
  name      text NOT NULL,
  display_kind text NOT NULL DEFAULT 'business' CHECK (display_kind IN ('person','business')),
  verified  boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (
    (kind = 'user' AND user_id IS NOT NULL AND org_id IS NULL) OR
    (kind = 'org' AND org_id IS NOT NULL AND user_id IS NULL) OR
    (kind = 'external' AND user_id IS NULL AND org_id IS NULL)
  )
);

CREATE TABLE notifications (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  type        text NOT NULL CHECK (type IN ('opportunity_detected','new_market','auction_invitation','outbid','winning_bid',
                                            'auction_ending','transaction_update','payment','delivery','reputation_update')),
  title       text NOT NULL,
  body        text NOT NULL,
  href        text NOT NULL,
  read_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notifications_user_idx ON notifications (user_id, created_at DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id) WHERE read_at IS NULL;

CREATE TABLE notification_prefs (
  user_id  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  type     text NOT NULL,
  in_app   boolean NOT NULL DEFAULT true,
  email    boolean NOT NULL DEFAULT true,
  PRIMARY KEY (user_id, type)
);

CREATE TABLE audit_log (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at           timestamptz NOT NULL DEFAULT now(),
  actor_user_id uuid REFERENCES users(id),
  actor_label  text NOT NULL,
  action       text NOT NULL,
  entity_type  text NOT NULL CHECK (entity_type IN ('user','business','opportunity','market','auction','alert','transaction',
                                                    'dispute','rule','procurement','supplier')),
  entity_id    text NOT NULL,
  entity_label text NOT NULL,
  org_id       uuid REFERENCES orgs(id),
  market_id    uuid,
  reason       text,
  changes      jsonb NOT NULL DEFAULT '[]'
);
CREATE INDEX audit_entity_idx ON audit_log (entity_type, entity_id, at DESC);
CREATE INDEX audit_org_idx ON audit_log (org_id, at DESC) WHERE org_id IS NOT NULL;
CREATE INDEX audit_market_idx ON audit_log (market_id, at DESC) WHERE market_id IS NOT NULL;
CREATE INDEX audit_at_idx ON audit_log (at DESC);

-- +goose StatementBegin
CREATE FUNCTION audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'audit_log is append-only';
END $$;
-- +goose StatementEnd
CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE ON audit_log FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();

CREATE TABLE outbox (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  created_at    timestamptz NOT NULL DEFAULT now(),
  topic         text NOT NULL,
  aggregate_id  text NOT NULL,
  payload       jsonb NOT NULL,
  published_at  timestamptz,
  attempts      int NOT NULL DEFAULT 0
);
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;

COMMENT ON TABLE users IS 'A person''s platform account (participant); capabilities and org memberships hang off it.';
COMMENT ON TABLE user_capabilities IS 'A capability granted on top of the participant account (market_maker, admin).';
COMMENT ON TABLE sessions IS 'A signed-in browser session behind the httpOnly cookie (token stored as SHA-256).';
COMMENT ON TABLE auth_tokens IS 'A single-use email-verification or password-reset token (hash only).';
COMMENT ON TABLE orgs IS 'An organization (business workspace); profile and settings live in the org migration.';
COMMENT ON TABLE org_members IS 'A member or pending invitee of an organization, with their role.';
COMMENT ON TABLE parties IS 'Anyone who can be one side of a trade, quote, contract or dispute: a user, an org, or an external party.';
COMMENT ON TABLE notifications IS 'An in-app notification for one user.';
COMMENT ON TABLE notification_prefs IS 'A user''s channel preference for one notification type (missing row = defaults).';
COMMENT ON TABLE audit_log IS 'An append-only record of a governance, market maker or organization action.';
COMMENT ON TABLE outbox IS 'A domain event waiting to be published to ClickHouse and the realtime hub (transactional outbox).';

-- +goose Down
DROP TABLE outbox;
DROP TRIGGER audit_log_no_update ON audit_log;
DROP FUNCTION audit_log_immutable();
DROP TABLE audit_log;
DROP TABLE notification_prefs;
DROP TABLE notifications;
DROP TABLE parties;
DROP TABLE org_members;
DROP TABLE orgs;
DROP TABLE auth_tokens;
DROP TABLE sessions;
DROP TABLE user_capabilities;
DROP TABLE users;
DROP DOMAIN category_id;
DROP DOMAIN qty;
DROP DOMAIN idr;
DROP FUNCTION next_code(text);
DROP SEQUENCE public_code_seq;
DROP FUNCTION set_updated_at();

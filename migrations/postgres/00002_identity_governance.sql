-- +goose Up

ALTER TABLE users ADD COLUMN suspended_at timestamptz;
ALTER TABLE users ADD CONSTRAINT users_suspended_at_check CHECK ((status = 'suspended') = (suspended_at IS NOT NULL));

CREATE TABLE identities (
  user_id            uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  bio                text NOT NULL DEFAULT '',
  availability_days  smallint[] NOT NULL DEFAULT '{0,1,2,3,4}'
                     CHECK (availability_days <@ '{0,1,2,3,4,5,6}'::smallint[]),
  available_from     time NOT NULL DEFAULT '08:00',
  available_to       time NOT NULL DEFAULT '17:00',
  pref_locations     text[] NOT NULL DEFAULT '{}',
  pref_categories    category_id[] NOT NULL DEFAULT '{}',
  min_price_idr      idr,
  max_budget_idr     idr,
  delivery_radius_km numeric(7,1) NOT NULL DEFAULT 25 CHECK (delivery_radius_km >= 0),
  identity_verified_at timestamptz,
  identity_verified_by uuid REFERENCES users(id),
  nik_hash           bytea UNIQUE,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CHECK ((identity_verified_at IS NULL) = (identity_verified_by IS NULL)),
  CHECK (nik_hash IS NULL OR identity_verified_at IS NOT NULL)
);
CREATE TRIGGER identities_updated_at BEFORE UPDATE ON identities FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE identities IS 'A participant''s economic identity (1:1 users): bio, availability, matching preferences, admin identity verification.';

CREATE TABLE capacity_items (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  position    smallint NOT NULL,
  kind        text NOT NULL CHECK (kind IN ('skill','asset','capacity','resource')),
  name        text NOT NULL CHECK (length(btrim(name)) > 0),
  detail      text NOT NULL DEFAULT '',
  category_id category_id,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, position)
);
CREATE TRIGGER capacity_items_updated_at BEFORE UPDATE ON capacity_items FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE capacity_items IS 'A skill, asset, capacity or resource a participant offers (Identity.items).';

CREATE TABLE phone_verifications (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  phone       text NOT NULL CHECK (phone ~ '^628[0-9]{7,11}$'),
  code_hash   bytea NOT NULL,
  attempts    int NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  expires_at  timestamptz NOT NULL,
  verified_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX phone_verifications_pending_key ON phone_verifications (user_id) WHERE verified_at IS NULL;
CREATE INDEX phone_verifications_verified_idx ON phone_verifications (user_id, verified_at DESC) WHERE verified_at IS NOT NULL;
CREATE TRIGGER phone_verifications_updated_at BEFORE UPDATE ON phone_verifications FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE phone_verifications IS 'One phone OTP challenge; verified rows are the proof of a verified phone (KYC level 1).';

CREATE TABLE verification_requests (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind          text NOT NULL CHECK (kind IN ('business','personal')),
  org_id        uuid REFERENCES orgs(id) ON DELETE CASCADE,
  submitted_by  uuid NOT NULL REFERENCES users(id),
  subject_name  text NOT NULL CHECK (length(btrim(subject_name)) > 0),
  form          jsonb NOT NULL DEFAULT '[]',
  status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected','reupload')),
  decided_by    uuid REFERENCES users(id),
  decided_at    timestamptz,
  decision_note text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CHECK ((kind = 'business') = (org_id IS NOT NULL)),
  CHECK ((status = 'pending') = (decided_at IS NULL)),
  CHECK ((decided_at IS NULL) = (decided_by IS NULL) AND (decided_at IS NULL) = (decision_note IS NULL))
);
CREATE UNIQUE INDEX verification_requests_pending_org_key ON verification_requests (org_id) WHERE status = 'pending' AND kind = 'business';
CREATE UNIQUE INDEX verification_requests_pending_user_key ON verification_requests (submitted_by) WHERE status = 'pending' AND kind = 'personal';
CREATE INDEX verification_requests_queue_idx ON verification_requests (created_at) WHERE status = 'pending';
CREATE INDEX verification_requests_list_idx ON verification_requests (created_at DESC);
CREATE INDEX verification_requests_org_idx ON verification_requests (org_id, created_at DESC) WHERE org_id IS NOT NULL;
CREATE INDEX verification_requests_user_idx ON verification_requests (submitted_by, created_at DESC);
CREATE TRIGGER verification_requests_updated_at BEFORE UPDATE ON verification_requests FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE verification_requests IS 'One business (org documents) or personal (KTP + selfie) verification request in the admin review queue.';

CREATE TABLE verification_documents (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  request_id  uuid NOT NULL REFERENCES verification_requests(id) ON DELETE CASCADE,
  kind        text NOT NULL CHECK (kind IN ('nib','npwp','akta','ktp','selfie')),
  file_name   text NOT NULL,
  object_key  text NOT NULL,
  fields      jsonb NOT NULL DEFAULT '[]',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (request_id, kind)
);
CREATE TRIGGER verification_documents_updated_at BEFORE UPDATE ON verification_documents FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE verification_documents IS 'One uploaded document of a verification request, with what OCR read from it.';

CREATE TABLE kyc_submissions (
  verification_request_id uuid PRIMARY KEY REFERENCES verification_requests(id) ON DELETE CASCADE,
  nik_encrypted bytea NOT NULL,
  nik_hash      bytea NOT NULL CHECK (octet_length(nik_hash) = 32),
  full_name     text NOT NULL CHECK (length(btrim(full_name)) > 0),
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX kyc_submissions_nik_idx ON kyc_submissions (nik_hash);
COMMENT ON TABLE kyc_submissions IS 'NIK (encrypted + keyed hash) and KTP name of a personal verification request.';

CREATE TABLE user_reports (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id           uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  reporter_party_id uuid NOT NULL REFERENCES parties(id),
  reason            text NOT NULL CHECK (length(btrim(reason)) > 0),
  context           text,
  created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX user_reports_user_idx ON user_reports (user_id, created_at DESC);
COMMENT ON TABLE user_reports IS 'A report filed against a user account (admin reportCount / reports).';

CREATE TABLE suspension_appeals (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  suspended_at  timestamptz NOT NULL,
  reason        text NOT NULL CHECK (length(btrim(reason)) >= 20),
  status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','granted','denied')),
  decided_by    uuid REFERENCES users(id),
  decided_at    timestamptz,
  decision_note text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, suspended_at),
  CHECK ((status = 'pending') = (decided_at IS NULL)),
  CHECK ((decided_at IS NULL) = (decided_by IS NULL) AND (decided_at IS NULL) = (decision_note IS NULL))
);
CREATE INDEX suspension_appeals_pending_idx ON suspension_appeals (created_at) WHERE status = 'pending';
CREATE TRIGGER suspension_appeals_updated_at BEFORE UPDATE ON suspension_appeals FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE suspension_appeals IS 'A suspended user''s appeal against one suspension, and the admin decision on it.';

CREATE TABLE mm_applications (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  organization    text NOT NULL CHECK (length(btrim(organization)) > 0),
  categories      category_id[] NOT NULL DEFAULT '{}',
  experience      text NOT NULL,
  documents       text NOT NULL DEFAULT '',
  status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected')),
  decided_by      uuid REFERENCES users(id),
  decided_at      timestamptz,
  decision_reason text,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CHECK ((status = 'pending') = (decided_at IS NULL)),
  CHECK ((decided_at IS NULL) = (decided_by IS NULL))
);
CREATE UNIQUE INDEX mm_applications_pending_key ON mm_applications (user_id) WHERE status = 'pending';
CREATE INDEX mm_applications_user_idx ON mm_applications (user_id, created_at DESC);
CREATE INDEX mm_applications_list_idx ON mm_applications (created_at DESC);
CREATE TRIGGER mm_applications_updated_at BEFORE UPDATE ON mm_applications FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE mm_applications IS 'An application to get the market_maker capability, and the admin decision on it.';

CREATE TABLE fraud_alerts (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code          text NOT NULL UNIQUE DEFAULT next_code('FRD'),
  type          text NOT NULL CHECK (type IN ('bid_manipulation','collusion','fake_accounts','abnormal_bidding','wash_trading',
                                              'price_manipulation','transaction_network')),
  source        text NOT NULL CHECK (source IN ('system','manual')),
  title         text NOT NULL,
  score         numeric(5,2) NOT NULL DEFAULT 0 CHECK (score BETWEEN 0 AND 100),
  confidence    numeric(4,3) NOT NULL DEFAULT 0 CHECK (confidence BETWEEN 0 AND 1),
  status        text NOT NULL DEFAULT 'new' CHECK (status IN ('new','investigating','escalated','dismissed','closed')),
  detected_at   timestamptz NOT NULL DEFAULT now(),
  evidence      text[] NOT NULL DEFAULT '{}',
  graph         jsonb,
  investigation_opened_at timestamptz,
  investigation_opened_by uuid REFERENCES users(id),
  resolved_at   timestamptz,
  resolved_by   uuid REFERENCES users(id),
  resolution_outcome text,
  resolution_reason  text,
  escalation    text CHECK (escalation IN ('suspend_user','restrict_user','freeze_auction','suspend_market')),
  escalated_subject_id uuid,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CHECK ((status = 'new') = (investigation_opened_at IS NULL)),
  CHECK ((investigation_opened_at IS NULL) = (investigation_opened_by IS NULL)),
  CHECK ((status IN ('escalated','dismissed','closed')) = (resolved_at IS NOT NULL)),
  CHECK ((resolved_at IS NULL) = (resolved_by IS NULL) AND (resolved_at IS NULL) = (resolution_outcome IS NULL)
         AND (resolved_at IS NULL) = (resolution_reason IS NULL)),
  CHECK ((status = 'escalated') = (escalation IS NOT NULL) AND (escalation IS NULL) = (escalated_subject_id IS NULL))
);
CREATE INDEX fraud_alerts_detected_idx ON fraud_alerts (detected_at DESC);
CREATE INDEX fraud_alerts_new_idx ON fraud_alerts (score DESC) WHERE status = 'new';
CREATE INDEX fraud_alerts_open_idx ON fraud_alerts (status) WHERE status IN ('new','investigating');
CREATE TRIGGER fraud_alerts_updated_at BEFORE UPDATE ON fraud_alerts FOR EACH ROW EXECUTE FUNCTION set_updated_at();
COMMENT ON TABLE fraud_alerts IS 'A fraud alert (engine recommendation or manual admin case) with its investigation and resolution.';

CREATE TABLE fraud_alert_subjects (
  alert_id      uuid NOT NULL REFERENCES fraud_alerts(id) ON DELETE CASCADE,
  subject_type  text NOT NULL CHECK (subject_type IN ('user','auction','market')),
  subject_id    uuid NOT NULL,
  label         text NOT NULL,
  PRIMARY KEY (alert_id, subject_id)
);
CREATE INDEX fraud_alert_subjects_subject_idx ON fraud_alert_subjects (subject_type, subject_id);
COMMENT ON TABLE fraud_alert_subjects IS 'A user, auction or market implicated in a fraud alert.';

ALTER TABLE fraud_alerts ADD CONSTRAINT fraud_alerts_escalated_subject_fkey
  FOREIGN KEY (id, escalated_subject_id) REFERENCES fraud_alert_subjects (alert_id, subject_id);

CREATE TABLE fraud_alert_notes (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  alert_id    uuid NOT NULL REFERENCES fraud_alerts(id) ON DELETE CASCADE,
  text        text NOT NULL CHECK (length(btrim(text)) > 0),
  created_by  uuid NOT NULL REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX fraud_alert_notes_alert_idx ON fraud_alert_notes (alert_id, created_at);
COMMENT ON TABLE fraud_alert_notes IS 'One note in a fraud alert investigation timeline.';

-- +goose Down
DROP TABLE fraud_alert_notes;
ALTER TABLE fraud_alerts DROP CONSTRAINT fraud_alerts_escalated_subject_fkey;
DROP TABLE fraud_alert_subjects;
DROP TABLE fraud_alerts;
DROP TABLE mm_applications;
DROP TABLE suspension_appeals;
DROP TABLE user_reports;
DROP TABLE kyc_submissions;
DROP TABLE verification_documents;
DROP TABLE verification_requests;
DROP TABLE phone_verifications;
DROP TABLE capacity_items;
DROP TABLE identities;
ALTER TABLE users DROP CONSTRAINT users_suspended_at_check;
ALTER TABLE users DROP COLUMN suspended_at;

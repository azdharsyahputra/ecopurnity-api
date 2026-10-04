-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION org_purchase_history_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  at       timestamptz := NEW.month::timestamp AT TIME ZONE 'UTC';
  opening  bigint := coalesce(NEW.opening_idr, NEW.market_unit_idr);
  auction  text := CASE WHEN NEW.via = 'auction' THEN NEW.id::text END;
BEGIN
  INSERT INTO outbox (created_at, topic, aggregate_id, payload) VALUES (at, 'trade.status', NEW.id::text, jsonb_build_object(
    'code', NEW.code, 'status', 'agreement', 'via', NEW.via, 'item', NEW.item, 'categoryId', NEW.category_id, 'region', '',
    'auctionId', auction, 'buyerPartyId', coalesce((SELECT id::text FROM parties WHERE org_id = NEW.org_id), ''), 'buyerOrgId', NEW.org_id,
    'supplierPartyId', coalesce((SELECT party_id::text FROM suppliers WHERE id = NEW.supplier_id), 's:' || NEW.supplier_id),
    'quantity', NEW.quantity, 'unit', NEW.unit, 'unitPriceIdr', NEW.unit_price_idr, 'valueIdr', round(NEW.unit_price_idr * NEW.quantity),
    'budgetUnitIdr', NEW.budget_unit_idr, 'marketUnitIdr', NEW.market_unit_idr));
  IF auction IS NOT NULL THEN
    INSERT INTO outbox (created_at, topic, aggregate_id, payload) VALUES (at, 'auction.closed', auction, jsonb_build_object(
      'code', NEW.code, 'title', NEW.item, 'orgId', NEW.org_id, 'categoryId', NEW.category_id, 'region', '', 'bidders', coalesce(NEW.bidders, 0),
      'openingIdr', opening, 'clearingIdr', NEW.unit_price_idr, 'demandIdr', round(NEW.quantity * opening), 'supplyIdr', 0, 'matchedIdr', 0));
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER org_purchase_history_outbox AFTER INSERT ON org_purchase_history FOR EACH ROW EXECUTE FUNCTION org_purchase_history_outbox();

-- +goose Down
DROP TRIGGER org_purchase_history_outbox ON org_purchase_history;
DROP FUNCTION org_purchase_history_outbox();

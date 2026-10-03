package server

import (
	"context"
	"fmt"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Read models for markets and opportunities, shared by every endpoint that embeds them (listing detail, explorer,
// public lists, ...). Callers pass a WHERE fragment over the alias m / o and its args.

const marketSelect = `
	SELECT m.id, m.code, m.name, m.category_id, m.region, m.objective, m.mechanism, m.status,
	       p.name, p.display_kind, p.verified, m.unit, m.demand_value, m.supply_value, m.price_min_idr, m.price_max_idr,
	       m.volume_30d_idr,
	       (SELECT count(*) FROM market_participants x WHERE x.market_id = m.id AND x.status = 'active' AND x.role = 'buyer'),
	       (SELECT count(*) FROM market_participants x WHERE x.market_id = m.id AND x.status = 'active' AND x.role = 'supplier'),
	       (SELECT count(*) FROM auctions a WHERE a.market_id = m.id AND a.status IN ('live','extended'))
	FROM markets m JOIN parties p ON p.id = m.maker_party_id`

func loadMarkets(ctx context.Context, q dbtx, where string, args ...any) ([]api.Market, error) {
	rows, err := q.Query(ctx, marketSelect+" WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Market{}
	for rows.Next() {
		var m api.Market
		var unit string
		var demand, supply float64
		var minIdr, maxIdr, vol int64
		var buyers, suppliers, active int64
		if err := rows.Scan(&m.Id, &m.Code, &m.Name, &m.CategoryId, &m.Region, &m.Objective, &m.Mechanism, &m.Status,
			&m.Maker.Name, &m.Maker.Kind, &m.Maker.Verified, &unit, &demand, &supply, &minIdr, &maxIdr, &vol,
			&buyers, &suppliers, &active); err != nil {
			return nil, err
		}
		m.Demand = api.Quantity{Value: demand, Unit: unit}
		m.Supply = api.Quantity{Value: supply, Unit: unit}
		m.PriceRange.MinIdr, m.PriceRange.MaxIdr, m.PriceRange.Unit = int(minIdr), int(maxIdr), unit
		m.Volume30dIdr, m.Buyers, m.Suppliers, m.ActiveAuctions = int(vol), int(buyers), int(suppliers), int(active)
		out = append(out, m)
	}
	return out, rows.Err()
}

const opportunitySelect = `
	SELECT o.id, o.code, o.title, o.kind, o.category_id, o.region, o.status, o.unit, o.demand_value, o.supply_value,
	       o.participant_count, o.potential_value_idr, o.suggested_mechanism, o.confidence, o.detected_at
	FROM opportunities o`

func loadOpportunities(ctx context.Context, q dbtx, where string, args ...any) ([]api.Opportunity, error) {
	rows, err := q.Query(ctx, opportunitySelect+" WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Opportunity{}
	for rows.Next() {
		var o api.Opportunity
		var unit string
		var demand, supply, confidence float64
		var participants int32
		var value int64
		var detected time.Time
		if err := rows.Scan(&o.Id, &o.Code, &o.Title, &o.Kind, &o.CategoryId, &o.Region, &o.Status, &unit, &demand, &supply,
			&participants, &value, &o.SuggestedMechanism, &confidence, &detected); err != nil {
			return nil, fmt.Errorf("scan opportunity: %w", err)
		}
		o.Demand = api.Quantity{Value: demand, Unit: unit}
		o.Supply = api.Quantity{Value: supply, Unit: unit}
		o.Participants, o.PotentialValueIdr, o.Confidence, o.DetectedAt = int(participants), int(value), confidence, detected
		out = append(out, o)
	}
	return out, rows.Err()
}

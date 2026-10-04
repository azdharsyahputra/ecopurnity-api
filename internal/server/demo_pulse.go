package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

const demoPulseLock = "ecopurnity.demo_pulse"

const demoAuctionSQL = `(EXISTS (SELECT 1 FROM users du WHERE du.id = a.owner_user_id AND du.email LIKE $1)
	OR EXISTS (SELECT 1 FROM orgs dorg JOIN users du ON du.id = dorg.created_by WHERE dorg.id = a.owner_org_id AND du.email LIKE $1)
	OR EXISTS (SELECT 1 FROM markets dm JOIN parties dp ON dp.id = dm.maker_party_id JOIN orgs dorg ON dorg.id = dp.org_id
	           JOIN users du ON du.id = dorg.created_by WHERE dm.id = a.market_id AND du.email LIKE $1))`

const demoPartySQL = `(EXISTS (SELECT 1 FROM users du WHERE du.id = %[1]s.user_id AND du.email LIKE $1)
	OR EXISTS (SELECT 1 FROM orgs dorg JOIN users du ON du.id = dorg.created_by WHERE dorg.id = %[1]s.org_id AND du.email LIKE $1))`

func (s *Server) RunDemoPulse(ctx context.Context, every time.Duration) {
	if !s.DemoPulse {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.DemoPulseTick(ctx, time.Now()); err != nil && ctx.Err() == nil && s.Log != nil {
			s.Log.Warn("demo pulse", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func demoPace(now time.Time) float64 {
	l := now.In(wib)
	p := 0.03
	switch h := l.Hour(); {
	case h >= 8 && h < 12, h >= 13 && h < 17:
		p = 1
	case h >= 12 && h < 13, h >= 17 && h < 21:
		p = 0.6
	case h >= 6 && h < 8:
		p = 0.25
	}
	switch l.Weekday() {
	case time.Saturday:
		p *= 0.5
	case time.Sunday:
		p *= 0.3
	}
	return p
}

type demoPulse struct {
	s   *Server
	ctx context.Context
	r   *rand.Rand
	now time.Time
	g   *demoGen
	did []string
}

func (s *Server) DemoPulseTick(ctx context.Context, now time.Time) error {
	conn, err := s.DB.Primary().Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, demoPulseLock).Scan(&locked); err != nil || !locked {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, demoPulseLock)
	r := rand.New(rand.NewPCG(uint64(now.UnixNano()), 7))
	p := &demoPulse{s: s, ctx: ctx, r: r, now: now, g: &demoGen{r: r, now: now}}
	if err := p.maintainRounds(); err != nil {
		return err
	}
	if err := p.closingBursts(); err != nil {
		return err
	}
	if p.r.Float64() >= 0.09*demoPace(now) {
		return nil
	}
	actions := []struct {
		w  float64
		fn func() error
	}{{0.42, p.bid}, {0.3, p.advanceTrade}, {0.1, p.listing}, {0.06, p.rfq}, {0.06, p.directOrder}, {0.06, p.dutchAccept}}
	x := p.r.Float64()
	for _, a := range actions {
		if x < a.w {
			return a.fn()
		}
		x -= a.w
	}
	return nil
}

func demoSession(ctx context.Context, userID, name string) context.Context {
	return context.WithValue(ctx, reqKey{}, &reqState{session: &session{UserID: userID, Name: name, Status: "active"}})
}

func ignorable(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

type demoUser struct{ ID, Name, Location string }

func (p *demoPulse) users(where string, args ...any) ([]demoUser, error) {
	rows, err := p.s.DB.Primary().Query(p.ctx, `
		SELECT u.id::text, u.name, coalesce(u.location, '') FROM users u JOIN identities i ON i.user_id = u.id
		WHERE u.email LIKE $1 AND u.status = 'active' AND u.email_verified_at IS NOT NULL AND `+where+` ORDER BY random() LIMIT 20`,
		append([]any{demoEmailLike}, args...)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (demoUser, error) {
		var u demoUser
		return u, r.Scan(&u.ID, &u.Name, &u.Location)
	})
}

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.IntN(len(xs))] }

type pulseAuction struct {
	ID, Type, Item, Unit string
	Opening, Current     int64
	Step                 int64
	Qty                  float64
	EndsAt               time.Time
}

func (p *demoPulse) liveAuctions(extra string) ([]pulseAuction, error) {
	rows, err := p.s.DB.Primary().Query(p.ctx, `
		SELECT a.id::text, a.type, a.lot_item, a.unit, a.opening_price_idr, coalesce(a.current_price_idr, a.opening_price_idr), greatest(a.min_step_idr, 1),
		       a.quantity::float8, a.ends_at
		FROM auctions a WHERE a.status IN ('live','extended') AND a.ends_at > now() + interval '1 minute' AND `+demoAuctionSQL+` AND `+extra+`
		ORDER BY a.ends_at`, demoEmailLike)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[pulseAuction])
}

func (p *demoPulse) closingBursts() error {
	as, err := p.liveAuctions(`a.type <> 'dutch' AND a.ends_at < now() + interval '25 minutes'`)
	if err != nil {
		return err
	}
	for _, a := range as {
		if p.r.Float64() < 0.3*math.Max(0.3, demoPace(p.now)) {
			if err := p.bidOn(a); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *demoPulse) bid() error {
	as, err := p.liveAuctions(`a.type <> 'dutch'`)
	if err != nil || len(as) == 0 {
		return err
	}
	return p.bidOn(pick(p.r, as))
}

func (p *demoPulse) bidder(auctionID string) (demoUser, bool, error) {
	prev, err := p.users(`i.identity_verified_at IS NOT NULL AND EXISTS (SELECT 1 FROM bids b WHERE b.auction_id = $2 AND b.bidder_user_id = u.id)
		AND NOT EXISTS (SELECT 1 FROM bids b WHERE b.auction_id = $2 AND b.bidder_user_id = u.id AND b.status = 'leading')`, auctionID)
	if err != nil {
		return demoUser{}, false, err
	}
	if len(prev) > 0 && p.r.Float64() < 0.7 {
		return pick(p.r, prev), true, nil
	}
	fresh, err := p.users(`i.identity_verified_at IS NOT NULL AND NOT EXISTS (SELECT 1 FROM auctions a WHERE a.id = $2 AND a.owner_user_id = u.id)
		AND NOT EXISTS (SELECT 1 FROM auctions a JOIN org_members om ON om.org_id = a.owner_org_id WHERE a.id = $2 AND om.user_id = u.id)`, auctionID)
	if err != nil || len(fresh) == 0 {
		return demoUser{}, false, err
	}
	return pick(p.r, fresh), true, nil
}

func (p *demoPulse) qualify(u demoUser, auctionID, item string) (context.Context, bool, error) {
	ctx := demoSession(p.ctx, u.ID, u.Name)
	_, err := p.s.QualifyForAuction(ctx, api.QualifyForAuctionRequestObject{Id: auctionID,
		Body: &api.PersonalQualificationInput{AcceptRules: true, DocumentName: "spesifikasi-" + strings.ReplaceAll(strings.ToLower(item), " ", "-") + ".pdf"}})
	if ignorable(err) {
		return ctx, false, nil
	}
	return ctx, err == nil, err
}

func (p *demoPulse) bidOn(a pulseAuction) error {
	u, ok, err := p.bidder(a.ID)
	if err != nil || !ok {
		return err
	}
	ctx, ok, err := p.qualify(u, a.ID, a.Item)
	if err != nil || !ok {
		return err
	}
	k := int64(pick(p.r, []int{1, 1, 1, 2, 3}))
	var price int64
	switch a.Type {
	case "sealed":
		price = a.Opening - a.Step*int64(p.r.IntN(int(math.Max(1, float64(a.Opening)*0.06/float64(a.Step))))+1)
	case "forward":
		price = a.Current + a.Step*k
		if a.Current == a.Opening {
			price = a.Opening
		}
		if float64(price) > float64(a.Opening)*1.12 {
			return nil
		}
	default:
		price = a.Current - a.Step*k
		if a.Current == a.Opening {
			price = a.Opening
		}
		if float64(price) < float64(a.Opening)*0.9 {
			return nil
		}
	}
	_, err = p.s.PlaceBid(ctx, api.PlaceBidRequestObject{Id: a.ID, Body: &api.PersonalBidInput{PriceIdr: int(price)}})
	if ignorable(err) {
		return nil
	}
	return err
}

func (p *demoPulse) dutchAccept() error {
	as, err := p.liveAuctions(`a.type = 'dutch' AND a.current_price_idr <= a.opening_price_idr * 0.93`)
	if err != nil || len(as) == 0 {
		return err
	}
	a := pick(p.r, as)
	us, err := p.users(`i.identity_verified_at IS NOT NULL`)
	if err != nil || len(us) == 0 {
		return err
	}
	ctx, ok, err := p.qualify(pick(p.r, us), a.ID, a.Item)
	if err != nil || !ok {
		return err
	}
	_, err = p.s.AcceptDutchPrice(ctx, api.AcceptDutchPriceRequestObject{Id: a.ID})
	if ignorable(err) {
		return nil
	}
	return err
}

func (p *demoPulse) maintainRounds() error {
	type mk struct{ ID, Name, Unit, Mech, Op, OpName string }
	rows, err := p.s.DB.Primary().Query(p.ctx, `
		SELECT m.id::text, m.name, m.unit, m.mechanism, mo.user_id::text, u.name
		FROM markets m JOIN parties dp ON dp.id = m.maker_party_id JOIN orgs dorg ON dorg.id = dp.org_id JOIN users du ON du.id = dorg.created_by
		JOIN LATERAL (SELECT user_id FROM market_operators WHERE market_id = m.id ORDER BY created_at LIMIT 1) mo ON true JOIN users u ON u.id = mo.user_id
		WHERE du.email LIKE $1 AND m.status = 'active'
		  AND NOT EXISTS (SELECT 1 FROM auctions a WHERE a.market_id = m.id AND a.status IN ('live','extended','scheduled','qualification'))`, demoEmailLike)
	if err != nil {
		return err
	}
	ms, err := pgx.CollectRows(rows, pgx.RowToStructByPos[mk])
	if err != nil {
		return err
	}
	for _, m := range ms {
		def := slices.IndexFunc(demoMarkets, func(d demoMarketDef) bool { return d.Name == m.Name })
		lotMin, lotMax, item := 10.0, 100.0, m.Name
		if def >= 0 {
			lotMin, lotMax, item = demoMarkets[def].LotMin, demoMarkets[def].LotMax, demoMarkets[def].Item
		}
		var ref float64
		if err := p.s.DB.Primary().QueryRow(p.ctx, `
			SELECT coalesce((SELECT coalesce(clearing_price_idr, current_price_idr, opening_price_idr) FROM auctions WHERE market_id = $1 AND round_no IS NOT NULL
			                 ORDER BY round_no DESC LIMIT 1), (price_min_idr + price_max_idr) / 2)::float8 FROM markets WHERE id = $1`, m.ID).Scan(&ref); err != nil {
			return err
		}
		qty := niceQty(lotMin + p.r.Float64()*(lotMax-lotMin))
		opening := nicePrice(ref * (1.02 + p.r.Float64()*0.04))
		if roundType[m.Mech] == "forward" {
			opening = nicePrice(ref * (0.94 + p.r.Float64()*0.04))
		}
		if roundType[m.Mech] == "dutch" {
			opening = nicePrice(ref * 1.08)
		}
		var start *time.Time
		if demoPace(p.now) < 0.5 {
			next := atWIB(p.now.Add(4*time.Hour), 9)
			start = &next
		}
		_, err := p.s.OpenMmRound(demoSession(p.ctx, m.Op, m.OpName), api.OpenMmRoundRequestObject{Id: m.ID, Body: &api.CreateRoundInput{
			Title: fmt.Sprintf("%s %s %s", item, idNumber(qty), m.Unit), Quantity: qty, OpeningPriceIdr: int(opening),
			DurationMinutes: (24 + p.r.IntN(48)) * 60, StartsAt: start}})
		if err != nil && !ignorable(err) {
			return err
		}
	}
	if demoPace(p.now) < 0.5 {
		return nil
	}
	type rd struct{ Market, Auction, Op, OpName string }
	rows, err = p.s.DB.Primary().Query(p.ctx, `
		SELECT a.market_id::text, a.id::text, mo.user_id::text, u.name FROM auctions a
		JOIN LATERAL (SELECT user_id FROM market_operators WHERE market_id = a.market_id ORDER BY created_at LIMIT 1) mo ON true JOIN users u ON u.id = mo.user_id
		WHERE a.round_no IS NOT NULL AND a.status = 'closed' AND a.type <> 'dutch' AND a.ends_at < now() - interval '2 hours' AND a.bid_count > 0
		  AND NOT EXISTS (SELECT 1 FROM settlements st WHERE st.auction_id = a.id) AND `+demoAuctionSQL, demoEmailLike)
	if err != nil {
		return err
	}
	rs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[rd])
	if err != nil {
		return err
	}
	for _, x := range rs {
		if p.r.Float64() > 0.2 {
			continue
		}
		if _, err := p.s.SettleMmRound(demoSession(p.ctx, x.Op, x.OpName), api.SettleMmRoundRequestObject{Id: x.Market, Aid: x.Auction}); err != nil && !ignorable(err) {
			return err
		}
	}
	return nil
}

func (p *demoPulse) advanceTrade() error {
	var id string
	err := p.s.DB.Primary().QueryRow(p.ctx, `
		SELECT t.id::text FROM trades t JOIN parties bp ON bp.id = t.buyer_party_id JOIN parties sp ON sp.id = t.supplier_party_id
		WHERE `+fmt.Sprintf(demoPartySQL, "bp")+` AND `+fmt.Sprintf(demoPartySQL, "sp")+`
		  AND (t.status IN ('agreement','invoiced','paid','fulfilling','delivered','accepted')
		       OR (t.status = 'completed' AND (SELECT count(*) FROM reviews r WHERE r.trade_id = t.id) < 2 AND t.updated_at > now() - interval '7 days'))
		  AND t.updated_at < now() - make_interval(hours => 2 + (abs(hashtext(t.id::text)) % 20))
		ORDER BY random() LIMIT 1`, demoEmailLike).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return p.s.inTx(p.ctx, func(tx pgx.Tx) error {
		t, err := lockTrade(p.ctx, tx, id)
		if err != nil {
			return err
		}
		st := t.state()
		order := []string{"accept_agreement", "issue_invoice", "pay", "ship", "upload_proof", "confirm_receipt", "review"}
		for _, side := range []string{"buyer", "supplier"} {
			open := tradeActions(st, side)
			for _, a := range order {
				if !slices.Contains(open, a) {
					continue
				}
				actor, err := p.actor(tx, t, side)
				if err != nil {
					return err
				}
				in := api.TradeActionInput{Action: api.TradeAction(a)}
				switch a {
				case "pay":
					actor.UserID = nil
				case "ship":
					var address string
					if err := tx.QueryRow(p.ctx, `SELECT delivery_address FROM trades WHERE id = $1`, id).Scan(&address); err != nil {
						return err
					}
					in.Shipment = &struct {
						Carrier     *string    `json:"carrier,omitempty"`
						DropPoint   string     `json:"dropPoint"`
						Quantity    float64    `json:"quantity"`
						ScheduledAt *time.Time `json:"scheduledAt,omitempty"`
					}{DropPoint: address, Quantity: st.UnscheduledQty, Carrier: ptr(pick(p.r, demoCarriers))}
				case "upload_proof":
					actor.File = &upload{FileName: "surat-jalan-" + strings.TrimPrefix(t.Code, "TRX-") + ".jpg"}
				case "review":
					rating := pick(p.r, []int{5, 5, 5, 4, 4, 3})
					cat := ""
					_ = tx.QueryRow(p.ctx, `SELECT coalesce(m.category_id, a.category_id, l.category_id, 'food') FROM trades t LEFT JOIN markets m ON m.id = t.market_id
						LEFT JOIN auctions a ON a.id = t.auction_id LEFT JOIN listings l ON l.id = t.source_listing_id WHERE t.id = $1`, id).Scan(&cat)
					text := p.g.reviewText(cat, strings.Split(t.Title, " · ")[0], rating, side == "buyer")
					sub := float64(rating)
					in.Review = &struct {
						Communication float64 `json:"communication"`
						Quality       float64 `json:"quality"`
						Rating        int     `json:"rating"`
						Text          string  `json:"text"`
						Timeliness    float64 `json:"timeliness"`
					}{sub, sub, rating, text, math.Max(1, sub-0.5)}
				}
				return applyTradeAction(p.ctx, tx, id, actor, in)
			}
		}
		return nil
	})
}

func (p *demoPulse) actor(tx pgx.Tx, t tradeRow, side string) (tradeActor, error) {
	a := tradeActor{Side: side, Name: t.PartyName[side], UserID: t.User[side]}
	if a.UserID != nil {
		return a, nil
	}
	var org, owner string
	err := tx.QueryRow(p.ctx, `SELECT p.org_id::text, o.created_by::text FROM parties p JOIN orgs o ON o.id = p.org_id WHERE p.id = $1`, t.Party[side]).Scan(&org, &owner)
	a.UserID, a.OrgID = &owner, &org
	return a, err
}

func (p *demoPulse) listing() error {
	us, err := p.users(`true`)
	if err != nil || len(us) == 0 {
		return err
	}
	u := pick(p.r, us)
	it := pick(p.r, demoItems)
	for _, x := range p.r.Perm(len(demoItems)) {
		if slices.ContainsFunc(demoItems[x].Cities, func(c string) bool { return regionOf(c) == regionOf(u.Location) }) {
			it = demoItems[x]
			break
		}
	}
	qty := niceQty(math.Max(it.QtyMin*0.5, (it.QtyMin+p.r.Float64()*(it.QtyMax-it.QtyMin))*0.35))
	price := nicePrice(float64(it.PriceMin) + p.r.Float64()*float64(it.PriceMax-it.PriceMin))
	var body api.ListingInput
	if p.r.Float64() < it.DemandBias {
		err = body.FromDemandListingInput(api.DemandListingInput{Item: it.Name, CategoryId: api.CategoryId(it.Category), Quantity: api.Quantity{Value: qty, Unit: it.Unit},
			Location: u.Location, Spec: pick(p.r, it.Specs), Delivery: api.DeliveryMode(it.Delivery), BudgetIdr: int(nicePrice(float64(price) * qty * 0.93)),
			Deadline: p.now.AddDate(0, 0, 14+p.r.IntN(30)), Attachments: []api.ListingAttachmentInput{}})
	} else {
		loc := u.Location
		for _, c := range it.Cities {
			if regionOf(c) == regionOf(u.Location) {
				loc = c
			}
		}
		err = body.FromSupplyListingInput(api.SupplyListingInput{Item: it.Name, CategoryId: api.CategoryId(it.Category), Quantity: api.Quantity{Value: qty, Unit: it.Unit},
			Location: loc, Spec: pick(p.r, it.Specs), Delivery: api.DeliveryMode(it.Delivery), PriceIdr: int(price), AvailableFrom: p.now,
			Attachments: []api.ListingAttachmentInput{}})
	}
	if err != nil {
		return err
	}
	_, err = p.s.CreateMyListing(demoSession(p.ctx, u.ID, u.Name), api.CreateMyListingRequestObject{Body: &body})
	if ignorable(err) {
		return nil
	}
	return err
}

func (p *demoPulse) rfq() error {
	us, err := p.users(`true`)
	if err != nil || len(us) == 0 {
		return err
	}
	u := pick(p.r, us)
	it := pick(p.r, demoItems)
	qty := niceQty((it.QtyMin + p.r.Float64()*(it.QtyMax-it.QtyMin)) * 0.4)
	target := int(nicePrice(float64(it.PriceMin+it.PriceMax) / 2 * 0.97))
	out, err := p.s.CreateRfq(demoSession(p.ctx, u.ID, u.Name), api.CreateRfqRequestObject{Body: &api.NewRfq{Item: it.Name, CategoryId: api.CategoryId(it.Category),
		Quantity: api.Quantity{Value: qty, Unit: it.Unit}, Deadline: p.now.AddDate(0, 0, 7+p.r.IntN(14)), Location: u.Location, Spec: pick(p.r, it.Specs),
		TargetPriceIdr: &target}})
	if ignorable(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if v, ok := out.(api.CreateRfq201JSONResponse); ok {
		_, err = p.s.DB.Primary().Exec(p.ctx, `DELETE FROM notifications n USING users u WHERE u.id = n.user_id AND u.email NOT LIKE $1 AND n.href = $2`,
			demoEmailLike, "/app/rfq/"+v.Id)
	}
	return err
}

func (p *demoPulse) directOrder() error {
	var market, listing string
	var qty float64
	err := p.s.DB.Primary().QueryRow(p.ctx, `
		SELECT l.market_id::text, l.id::text, l.quantity::float8 FROM listings l JOIN parties lp ON lp.id = l.owner_party_id JOIN markets m ON m.id = l.market_id
		WHERE m.mechanism = 'direct_market' AND m.status = 'active' AND l.kind = 'supply' AND l.status IN ('available','in_market') AND l.quantity > 0
		  AND `+fmt.Sprintf(demoPartySQL, "lp")+` ORDER BY random() LIMIT 1`, demoEmailLike).Scan(&market, &listing, &qty)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	us, err := p.users(`i.identity_verified_at IS NOT NULL`)
	if err != nil || len(us) == 0 {
		return err
	}
	u := pick(p.r, us)
	_, err = p.s.CreateDirectMarketOrder(demoSession(p.ctx, u.ID, u.Name), api.CreateDirectMarketOrderRequestObject{Id: market,
		Body: &api.AuthPublicDirectOrderRequest{ListingId: listing, Quantity: math.Max(1, math.Round(qty*(0.1+p.r.Float64()*0.3)))}})
	if ignorable(err) {
		return nil
	}
	return err
}

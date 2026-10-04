package server

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

type demoAuction struct {
	ID, Code, Title, Category, Type, Status, Visibility, Item, Spec, Unit, Region string
	Market                                                                        *demoMarket
	Round                                                                         int
	RuleVersion                                                                   *string
	OwnerUser                                                                     *string
	OwnerOrg                                                                      *demoOrg
	Qty                                                                           float64
	Opening, Step                                                                 int64
	Current, Clearing, Median                                                     *int64
	Start, End, Created                                                           time.Time
	Rules                                                                         []api.LabeledValue
	Invitees                                                                      []string
	CreatedBy                                                                     string
	Ext                                                                           bool
	Extensions                                                                    int
	Ref                                                                           float64
	Bids                                                                          []*demoBid
}

type demoBid struct {
	ID       string
	Party    *demoParty
	Price    int64
	At       time.Time
	Status   string
	Capacity *float64
}

func (a *demoAuction) lower() bool { return a.Type == "reverse" || a.Type == "sealed" }
func (a *demoAuction) owned() bool { return a.OwnerUser != nil || a.OwnerOrg != nil }

func (a *demoAuction) marketID() *string {
	if a.Market == nil {
		return nil
	}
	return &a.Market.ID
}

func (g *demoGen) ladder(a *demoAuction, bidders []*demoParty, n int, until time.Time, target float64) {
	if len(bidders) == 0 || !until.After(a.Start) {
		return
	}
	pool := bidders[:min(len(bidders), g.i(3, 12))]
	serious := pool[:min(len(pool), g.i(2, 3))]
	closing := !until.Before(a.End)
	span := until.Sub(a.Start)
	times := make([]time.Time, n)
	for k := range times {
		if closing && k >= n*2/3 {
			times[k] = until.Add(-time.Duration(g.f(5, 40*60)) * time.Second)
		} else {
			times[k] = a.Start.Add(time.Duration(float64(span) * (0.05 + 0.9*math.Pow(g.r.Float64(), 0.7))))
		}
		for try_ := 0; try_ < 6; try_++ {
			if h := times[k].In(wib).Hour(); h >= 7 && h < 21 {
				break
			}
			times[k] = a.Start.Add(time.Duration(float64(span) * g.r.Float64()))
		}
		times[k] = times[k].Truncate(time.Second)
	}
	slices.SortFunc(times, func(x, y time.Time) int { return x.Compare(y) })
	cur := float64(a.Opening)
	var lastParty *demoParty
	for k := range n {
		var p *demoParty
		for p == nil || (p == lastParty && len(pool) > 1) {
			if g.chance(0.5) {
				p = demoPick(g, serious)
			} else {
				p = demoPick(g, pool)
			}
		}
		lastParty = p
		var price int64
		if a.Type == "sealed" {
			price = min(a.Opening, nicePrice(target*g.f(0.97, 1.04)))
		} else {
			price = int64(cur) + a.Step*int64(math.Round((target-cur)*g.f(0.08, 0.3)/float64(a.Step)))
			if a.lower() {
				limit := int64(cur)
				if k > 0 {
					limit -= a.Step
				}
				price = min(price, limit)
			} else {
				limit := int64(cur)
				if k > 0 {
					limit += a.Step
				}
				price = max(price, limit)
			}
			if (a.lower() && float64(price) < target-float64(a.Step)) || (!a.lower() && float64(price) > target+float64(a.Step)) {
				if k >= 2 {
					break
				}
			}
			cur = float64(price)
		}
		if price <= 0 {
			break
		}
		a.Bids = append(a.Bids, &demoBid{ID: newID(), Party: p, Price: price, At: times[k]})
	}
	if closing && a.Ext {
		for _, b := range a.Bids {
			if a.End.Sub(b.At) < 2*time.Minute && a.Extensions < maxExtensions {
				a.End = a.End.Add(5 * time.Minute)
				a.Extensions++
			}
		}
	}
}

func (a *demoAuction) offers() []*demoBid {
	best := map[*demoParty]*demoBid{}
	for _, b := range a.Bids {
		if c, ok := best[b.Party]; !ok || (a.lower() && b.Price < c.Price) || (!a.lower() && b.Price > c.Price) {
			best[b.Party] = b
		}
	}
	var out []*demoBid
	for _, b := range best {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Price != out[j].Price {
			return (out[i].Price < out[j].Price) == a.lower()
		}
		return out[i].At.Before(out[j].At)
	})
	return out
}

func (a *demoAuction) finishBids(winners map[*demoParty]bool) {
	os := a.offers()
	if len(os) > 0 {
		c := os[0].Price
		a.Current = &c
	}
	switch {
	case a.Status == "awarded" || (a.Status == "closed" && !a.owned() && len(os) > 0):
		if winners == nil {
			winners = map[*demoParty]bool{os[0].Party: true}
		}
		for _, b := range a.Bids {
			b.Status = map[bool]string{true: "won", false: "lost"}[winners[b.Party]]
		}
	case a.Type == "sealed":
		latest := map[*demoParty]*demoBid{}
		for _, b := range a.Bids {
			b.Status = "outbid"
			latest[b.Party] = b
		}
		for _, b := range latest {
			b.Status = "submitted"
		}
	default:
		for _, b := range a.Bids {
			b.Status = "outbid"
		}
		if len(a.Bids) > 0 {
			a.Bids[len(a.Bids)-1].Status = "leading"
		}
	}
	if (a.Status == "closed" || a.Status == "awarded") && len(os) > 0 {
		c, m := os[0].Price, os[len(os)/2].Price
		a.Clearing, a.Median = &c, &m
	}
}

func (g *demoGen) writeAuction(a *demoAuction) {
	if a.ID == "" {
		a.ID = newID()
	}
	if a.Code == "" {
		a.Code = g.code("AUC")
	}
	rules, _ := json.Marshal(a.Rules)
	var roundNo, owner, org any
	if a.Round > 0 {
		roundNo = a.Round
	}
	if a.OwnerUser != nil {
		owner = *a.OwnerUser
	}
	if a.OwnerOrg != nil {
		org = a.OwnerOrg.ID
	}
	current := a.Current
	if current == nil && (a.Visibility == "full" || a.Type == "dutch") {
		current = &a.Opening
	}
	ext, extMin := 2, 5
	if !a.Ext {
		ext, extMin = 0, 0
	}
	g.ins("auctions", "id, code, title, market_id, round_no, rule_version_id, owner_user_id, owner_org_id, category_id, type, status, visibility, lot_item, "+
		"lot_spec, quantity, unit, opening_price_idr, current_price_idr, min_step_idr, starts_at, ends_at, ext_window_minutes, ext_minutes, extension_count, rules, invitees, "+
		"clearing_price_idr, median_price_idr, created_by, created_at, updated_at",
		a.ID, a.Code, a.Title, a.marketID(), roundNo, a.RuleVersion, owner, org, a.Category, a.Type, a.Status, a.Visibility, a.Item, a.Spec, a.Qty, a.Unit,
		a.Opening, current, a.Step, a.Start, a.End, ext, extMin, a.Extensions, rules, nonNil(a.Invitees), a.Clearing, a.Median, a.CreatedBy, a.Created, a.Created)
	for _, b := range a.Bids {
		g.ins("bids", "id, auction_id, seq, bidder_party_id, bidder_user_id, bidder_no, price_idr, status, capacity, created_at, updated_at",
			b.ID, a.ID, 0, b.Party.ID, b.Party.UserID, 0, b.Price, b.Status, b.Capacity, b.At, b.At)
		if a.Type == "dutch" {
			continue
		}
		g.fact("auction.bid", a.ID, b.At, map[string]any{"marketId": a.marketID(), "categoryId": a.Category, "region": "", "auctionType": a.Type,
			"priceIdr": b.Price, "minStepIdr": a.Step, "bidderPartyId": b.Party.ID})
		var value *int64
		if a.Visibility == "full" {
			value = idrPtr(round(float64(b.Price) * a.Qty))
		}
		g.activity("bid_placed", "Bid baru di auction "+a.Title, value, a.marketID(), b.At)
	}
	if a.Status == "closed" || a.Status == "awarded" {
		var org any
		if a.OwnerOrg != nil {
			org = a.OwnerOrg.ID
		}
		g.fact("auction.closed", a.ID, a.End, map[string]any{"code": a.Code, "title": a.Title, "marketId": a.marketID(), "orgId": org,
			"categoryId": a.Category, "bidders": len(a.offers()), "openingIdr": a.Opening, "clearingIdr": a.Clearing,
			"demandIdr": int64(a.Qty * float64(a.Opening)), "supplyIdr": 0, "matchedIdr": 0})
		g.activity("auction_closed", "Auction ditutup: "+a.Title, nil, a.marketID(), a.End)
	}
}

func (g *demoGen) roundResult(a *demoAuction, at time.Time) {
	p := map[string]any{"round": a.Round, "auctionId": a.ID, "title": a.Title, "status": "closed", "at": a.Start, "openingIdr": a.Opening}
	if a.Status == "live" {
		p["status"] = "live"
		if a.Type != "sealed" && a.Current != nil {
			p["currentIdr"] = *a.Current
		}
	} else if a.Clearing != nil {
		p["clearingIdr"] = *a.Clearing
	} else if a.Current != nil {
		p["clearingIdr"] = *a.Current
	}
	if a.Median != nil {
		p["medianIdr"] = *a.Median
	} else if a.Current != nil {
		p["medianIdr"] = (a.Opening + *a.Current) / 2
	}
	g.fact("market.round_result", a.Market.ID, at, p)
}

func (g *demoGen) seedAuctions() {
	for _, m := range g.markets {
		g.marketRounds(m)
	}
	g.buyerAuctions()
	g.orgAuctions()
}

func atWIB(t time.Time, hour int) time.Time {
	l := t.In(wib)
	return time.Date(l.Year(), l.Month(), l.Day(), hour, 0, 0, 0, wib).UTC()
}

func (g *demoGen) newRound(m *demoMarket, round int, start, end time.Time, status string) *demoAuction {
	d, it := m.Def, m.Item
	v := m.version(round)
	m.Ref *= 1 + g.f(-0.024, 0.024)
	qty := niceQty(g.f(d.LotMin, d.LotMax))
	opening := nicePrice(m.Ref * g.f(1.02, 1.06))
	if m.Type == "forward" {
		opening = nicePrice(m.Ref * g.f(0.94, 0.98))
	}
	visibility := v.Rules.Visibility
	if m.Type == "sealed" {
		visibility = "sealed"
	}
	a := &demoAuction{Title: fmt.Sprintf("%s %s %s", it.Name, idNumber(qty), it.Unit), Category: it.Category, Type: m.Type, Status: status,
		Visibility: visibility, Item: it.Name, Spec: demoPick(g, it.Specs), Unit: it.Unit, Region: d.Region, Market: m, Round: round,
		RuleVersion: &v.ID, Qty: qty, Opening: opening, Step: niceStep(float64(opening) * v.Rules.MinStepPct / 100),
		Start: start, End: end, Created: start, CreatedBy: m.Op.ID, Ext: true, Ref: m.Ref}
	stepLabel := "Langkah minimum"
	if a.Type == "dutch" {
		stepLabel = "Penurunan harga"
	}
	a.Rules = []api.LabeledValue{
		{Label: "Round", Value: fmt.Sprintf("%d di %s", round, d.Name)},
		{Label: "Visibilitas bid", Value: ruleVisibility[visibility]},
		{Label: stepLabel, Value: fmt.Sprintf("%s per %s", rupiah(a.Step), it.Unit)},
		{Label: "Perpanjangan otomatis", Value: "+5 menit jika ada bid di 2 menit terakhir"},
	}
	for _, l := range v.Rules.labeled(it.Unit) {
		if l.Label == "Eligibility" || l.Label == "Penetapan pemenang" || l.Label == "Wilayah" {
			a.Rules = append(a.Rules, l)
		}
	}
	return a
}

func (g *demoGen) marketRounds(m *demoMarket) {
	d := m.Def
	bidders, members := m.Suppliers, m.Buyers
	if m.Type == "forward" || m.Type == "dutch" {
		bidders, members = m.Buyers, m.Suppliers
	}
	if d.Status == "formation" {
		start := atWIB(g.now.Add(time.Duration(g.i(2, 4))*day), 9)
		a := g.newRound(m, 1, start, start.Add(time.Duration(d.DurationDays)*day), "qualification")
		a.Created = g.ago(g.f(0.5, 2))
		g.writeAuction(a)
		g.activity("auction_started", fmt.Sprintf("Round 1 dibuka: %s", a.Title), idrPtr(round(float64(a.Opening)*a.Qty)), &m.ID, a.Created)
		return
	}
	cadence, dur := time.Duration(d.CadenceDays)*day, time.Duration(d.DurationDays)*day
	liveStart := g.now.Add(-hrs(g.f(3, 30)))
	if d.Status == "paused" {
		liveStart = g.now.Add(-hrs(g.f(30, 60)))
	}
	first := atWIB(maxTime(m.Created.Add(3*day), g.from.Add(-cadence/2)), 9)
	wd := time.Weekday(1 + slices.Index(g.markets, m)%4)
	for first.In(wib).Weekday() != wd {
		first = first.Add(day)
	}
	rn := 0
	var past []*demoAuction
	for start := first; !start.Add(dur).After(liveStart.Add(-6 * time.Hour)); start = start.Add(cadence) {
		rn++
		a := g.newRound(m, rn, start, start.Add(dur).Add(-hrs(16)), "closed")
		past = append(past, a)
	}
	poolRound := d.Mech == "collective_procurement" && d.Item == "Standing pouch zipper 250 g" && len(past) > 2
	for k, a := range past {
		settle := g.workHours(a.End.Add(hrs(g.f(1, 40))))
		awarded := settle.Before(g.now) && (k < len(past)-1 || g.chance(0.5))
		if m.Type == "dutch" {
			g.dutchRound(m, a, bidders)
			continue
		}
		target := a.Ref * g.f(0.96, 0.995)
		if m.Type == "forward" {
			target = a.Ref * g.f(1.005, 1.04)
		}
		g.ladder(a, bidders, g.i(6, 16), a.End, target)
		if awarded && len(a.Bids) > 0 {
			a.Status = "awarded"
		}
		a.finishBids(nil)
		g.writeAuction(a)
		g.activity("auction_started", fmt.Sprintf("Round %d dibuka: %s", a.Round, a.Title), idrPtr(round(float64(a.Opening)*a.Qty)), &m.ID, a.Start)
		if a.Status == "awarded" {
			if poolRound && k == 0 {
				g.poolSettlement(m, a, settle)
			} else {
				g.settleRound(m, a, members, settle)
			}
			g.roundResult(a, settle)
		} else {
			g.roundResult(a, a.End)
		}
	}
	if d.Status == "paused" {
		return
	}
	rn++
	live := g.newRound(m, rn, liveStart, g.now.Add(hrs(g.f(4, 60))), "live")
	if m.Type == "dutch" {
		live.Opening = nicePrice(live.Ref * 1.08)
		live.Current = idrPtr(live.Opening - int64(g.i(1, 6))*live.Step)
	} else {
		target := live.Ref * 0.97
		if m.Type == "forward" {
			target = live.Ref * 1.03
		}
		g.ladder(live, bidders, g.i(4, 12), g.now.Add(-5*time.Minute), target)
		live.finishBids(nil)
	}
	g.writeAuction(live)
	g.activity("auction_started", fmt.Sprintf("Round %d dibuka: %s", live.Round, live.Title), idrPtr(round(float64(live.Opening)*live.Qty)), &m.ID, live.Start)
	g.roundResult(live, live.Start)
	rn++
	nextStart := atWIB(live.End.Add(time.Duration(g.i(2, 5))*day), 9)
	status := "scheduled"
	if m.version(rn).Rules.Eligibility != "open" {
		status = "qualification"
	}
	next := g.newRound(m, rn, nextStart, nextStart.Add(dur), status)
	next.Created = g.ago(g.f(0.1, 1))
	g.writeAuction(next)
	g.activity("auction_started", fmt.Sprintf("Round %d dibuka: %s", next.Round, next.Title), idrPtr(round(float64(next.Opening)*next.Qty)), &m.ID, next.Created)
}

func (g *demoGen) dutchRound(m *demoMarket, a *demoAuction, buyers []*demoParty) {
	g.activity("auction_started", fmt.Sprintf("Round %d dibuka: %s", a.Round, a.Title), idrPtr(round(float64(a.Opening)*a.Qty)), &m.ID, a.Start)
	if len(buyers) == 0 || g.chance(0.15) {
		a.Status, a.Current = "closed", idrPtr(nicePrice(float64(a.Opening)*0.8))
		g.writeAuction(a)
		g.roundResult(a, a.End)
		return
	}
	price := a.Opening - int64(g.i(3, 14))*a.Step
	at := a.Start.Add(time.Duration(float64(a.End.Sub(a.Start)) * g.f(0.2, 0.9)))
	buyer := demoPick(g, buyers)
	a.Status, a.Current, a.Clearing = "awarded", &price, &price
	a.Bids = []*demoBid{{ID: newID(), Party: buyer, Price: price, At: at, Status: "won"}}
	g.writeAuction(a)
	g.roundResult(a, at)
	g.trade(&demoTrade{Title: fmt.Sprintf("%s %s %s", a.Item, qtyLabel(a.Qty), a.Unit), Buyer: buyer, Supplier: m.Maker.Party, Qty: a.Qty, Unit: a.Unit,
		Price: price, Maker: m.Maker.Party, MarketID: &m.ID, AuctionID: &a.ID, Address: buyer.Location, Start: at.Add(time.Minute), Via: "dutch",
		Category: a.Category, Region: m.Def.Region})
}

func (g *demoGen) settleRound(m *demoMarket, a *demoAuction, members []*demoParty, at time.Time) {
	winner := a.offers()[0]
	var ms []member
	var parties []*demoParty
	for _, p := range g.pickParties(members, g.i(2, 4), winner.Party) {
		ms = append(ms, member{ID: p.ID, Quantity: g.f(1, 10)})
		parties = append(parties, p)
	}
	g.settlement(m, a, at, winner, parties, splitProRata(a.Qty, ms), "Kolektif "+a.Code, a.Item, nil)
}

func (g *demoGen) settlement(m *demoMarket, a *demoAuction, at time.Time, winner *demoBid, parties []*demoParty, split []share, group, item string,
	lineIDs []string) string {
	side := "selling"
	if a.lower() {
		side = "procurement"
	}
	sid := newID()
	g.ins("settlements", "id, auction_id, side, winner_party_id, price_idr, settled_by, settled_at", sid, a.ID, side, winner.Party.ID, winner.Price, m.Op.ID, at)
	for k, p := range parties {
		if split[k].Quantity <= 0 {
			continue
		}
		t := &demoTrade{Title: fmt.Sprintf("%s · %s %s (%s)", item, idNumber(split[k].Quantity), a.Unit, a.Code), Buyer: p, Supplier: winner.Party,
			Qty: split[k].Quantity, Unit: a.Unit, Price: winner.Price, Maker: m.Maker.Party, MarketID: &m.ID, AuctionID: &a.ID, GroupLabel: &group,
			GroupShare: math.Round(split[k].Share*100000) / 100000, Address: p.Location, Start: at.Add(time.Duration(k) * time.Minute), Via: "settlement",
			Category: a.Category, Region: m.Def.Region}
		if side == "selling" {
			t.Buyer, t.Supplier = winner.Party, p
		}
		g.trade(t)
		id := newID()
		if lineIDs != nil {
			id = lineIDs[k]
		}
		g.ins("settlement_lines", "id, settlement_id, member_party_id, quantity, share, amount_idr, trade_id, created_at", id, sid, p.ID, split[k].Quantity,
			t.GroupShare, int64(split[k].Quantity)*winner.Price, t.ID, at)
	}
	return sid
}

func (g *demoGen) poolSettlement(m *demoMarket, a *demoAuction, at time.Time) {
	orgs := g.buyerOrgs(a.Category)
	if len(orgs) > 4 {
		orgs = orgs[:4]
	}
	winner := a.offers()[0]
	var ms []member
	var parties []*demoParty
	var lines []string
	for _, o := range orgs {
		if o.Party == winner.Party {
			continue
		}
		ms = append(ms, member{ID: o.ID, Quantity: g.f(1, 6)})
		parties = append(parties, o.Party)
		lines = append(lines, newID())
	}
	if len(parties) == 0 {
		g.settleRound(m, a, m.Buyers, at)
		return
	}
	split := splitProRata(a.Qty, ms)
	pool := newID()
	title := a.Item
	created := a.Start.Add(-10 * day)
	sid := g.settlement(m, a, at, winner, parties, split, "Pool kolektif "+a.Code, title, lines)
	g.ins("collective_pools", "id, title, category_id, spec, region, deadline, unit, base_unit_price_idr, ref_qty, threshold_qty, status, market_requested_at, "+
		"market_id, auction_id, formed_by, formed_at, settlement_id, created_by, created_at, updated_at", pool, title, a.Category, a.Spec, m.Def.Region,
		a.Start.Add(-day), a.Unit, a.Opening, niceQty(a.Qty/8), niceQty(a.Qty), "settled", created.Add(5*day), m.ID, a.ID, m.Op.ID, a.Start, sid,
		orgs[0].Owner.ID, created, at)
	for k, p := range parties {
		o := orgs[slices.IndexFunc(orgs, func(o *demoOrg) bool { return o.Party == p })]
		var line any
		if split[k].Quantity > 0 {
			line = lines[k]
		}
		joined := created.Add(hrs(float64(k) * 20))
		g.ins("pool_members", "pool_id, org_id, quantity, opt_in, drop_point, settlement_line_id, created_at, updated_at", pool, o.ID, niceQty(ms[k].Quantity/sum(ms)*a.Qty),
			k%2 == 0, o.Def.City, line, joined, joined)
		g.procurement(o, demoItemByName(a.Item), niceQty(ms[k].Quantity/sum(ms)*a.Qty), "po_issued", &pool, joined)
	}
}

func sum(ms []member) float64 {
	var s float64
	for _, m := range ms {
		s += m.Quantity
	}
	return s
}

func (g *demoGen) buyerAuctions() {
	n := 0
	for _, l := range g.listings {
		if n >= g.sc.BuyerAuctions {
			break
		}
		if l.Kind != "demand" || l.Owner.Kind != "user" || (l.Status != "open" && l.Status != "matched") || l.MarketID != nil {
			continue
		}
		age := g.now.Sub(l.Created)
		if age < 8*time.Hour {
			continue
		}
		n++
		typ := "reverse"
		visibility := []string{"full", "full", "rank_only"}[n%3]
		if n%4 == 0 {
			typ, visibility = "sealed", "sealed"
		}
		price := float64(l.Budget) / l.Qty
		opening := nicePrice(price * g.f(1.0, 1.05))
		start := l.Created.Add(hrs(g.f(2, math.Min(48, age.Hours()/2))))
		a := &demoAuction{Title: fmt.Sprintf("%s %s %s", l.Item, qtyLabel(l.Qty), l.Unit), Category: l.Category, Type: typ, Visibility: visibility,
			Item: l.Item, Spec: nonEmpty(l.Spec, "Sesuai deskripsi demand"), Unit: l.Unit, Region: regionOf(l.Location), OwnerUser: l.Owner.UserID, Qty: l.Qty,
			Opening: opening, Step: niceStep(float64(opening) * 0.005), Start: start, Created: start, CreatedBy: *l.Owner.UserID, Ext: true}
		invites := "Terbuka untuk supplier terkualifikasi"
		a.Rules = []api.LabeledValue{
			{Label: "Tipe", Value: map[string]string{"reverse": "Reverse auction", "sealed": "Sealed bid"}[typ]},
			{Label: "Harga pembuka", Value: fmt.Sprintf("%s per %s", rupiah(opening), l.Unit)},
			{Label: "Penurunan minimum", Value: rupiah(a.Step)},
			{Label: "Undangan", Value: invites},
			{Label: "Perpanjangan", Value: fmt.Sprintf("+5 menit jika ada bid di 2 menit terakhir (maks. %d kali)", maxExtensions)},
		}
		end := start.Add(hrs(g.f(24, 96)))
		switch {
		case end.Before(g.now.Add(-6*time.Hour)) && g.chance(0.75):
			a.Status = "awarded"
		case end.Before(g.now):
			a.Status = "closed"
		default:
			a.Status, end = "live", g.now.Add(hrs(g.f(3, 72)))
		}
		a.End = end
		bidders := g.pickParties(g.partiesFor(l.Category, "supply"), g.i(3, 6), l.Owner)
		g.ladder(a, bidders, g.i(4, 12), minTime(a.End, g.now.Add(-10*time.Minute)), price*g.f(0.93, 0.98))
		if len(a.Bids) == 0 {
			continue
		}
		var lines []*demoBid
		winners := map[*demoParty]bool{}
		if a.Status == "awarded" {
			os := a.offers()
			lines = os[:1]
			if len(os) > 1 && g.chance(0.3) {
				lines = os[:2]
			}
			for _, b := range lines {
				winners[b.Party] = true
			}
		}
		a.finishBids(winners)
		g.writeAuction(a)
		code := a.Code
		l.AuctionID = &a.ID
		l.Status = "in_market"
		l.Events = append(l.Events[:0], [2]any{"in_market", "Auction " + code + " dibuat"})
		l.Updated = start
		if a.Status != "awarded" {
			continue
		}
		awardAt := g.workHours(a.End.Add(hrs(g.f(1, 5))))
		if awardAt.After(g.now) {
			awardAt = a.End.Add(time.Hour)
		}
		l.Status, l.Updated = "matched", awardAt
		l.Events = append(l.Events, [2]any{"matched", fmt.Sprintf("Award ke %d supplier", len(lines))})
		award := newID()
		g.ins("auction_awards", "id, auction_id, awarded_by, created_at", award, a.ID, *l.Owner.UserID, awardAt)
		left := a.Qty
		for k, b := range lines {
			qty := left
			if k < len(lines)-1 {
				qty = niceQty(a.Qty * 0.6)
			}
			left -= qty
			g.ins("auction_award_lines", "award_id, bid_id, quantity, price_idr", award, b.ID, qty, b.Price)
			g.trade(&demoTrade{Title: fmt.Sprintf("%s %s %s", a.Item, qtyLabel(qty), a.Unit), Buyer: l.Owner, Supplier: b.Party, Qty: qty, Unit: a.Unit,
				Price: b.Price, AuctionID: &a.ID, Address: l.Location, Start: awardAt.Add(time.Duration(k) * time.Minute), Via: "auction",
				Category: a.Category, Region: l.Location})
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (g *demoGen) orgAuctions() {
	statuses := []string{"awarded", "live", "closed", "awarded", "scheduled", "pending_approval", "awarded", "live"}
	var sellers []*demoOrg
	for _, o := range g.orgs {
		if o.SupplierID != "" {
			sellers = append(sellers, o)
		}
	}
	for k := range g.sc.OrgAuctions {
		if len(g.orgs) == 0 {
			return
		}
		o := g.orgs[(k*3)%len(g.orgs)]
		status := statuses[k%len(statuses)]
		objective, typ := "procurement", []string{"reverse", "reverse", "sealed"}[k%3]
		cats := o.Def.Buy
		if k%5 == 4 && len(o.Def.Supply) > 0 {
			objective, typ, cats = "selling", "forward", o.Def.Supply
		}
		items := itemsIn(cats[0])
		lots := 1
		if k%4 == 1 {
			lots = 2
		}
		var created, start time.Time
		duration := g.i(1, 3) * 24 * 60
		switch status {
		case "awarded", "closed":
			start = g.ago(g.f(6, float64(g.days)*0.8))
			if status == "closed" {
				start = g.now.Add(-time.Duration(duration)*time.Minute - hrs(g.f(4, 30)))
			}
		case "live":
			start = g.now.Add(-hrs(g.f(2, float64(duration)/60-3)))
		default:
			start = g.now.Add(hrs(g.f(24, 96)))
		}
		created = start.Add(-hrs(g.f(4, 48)))
		by := o.member("procurement")
		id, code := newID(), g.code("OAU")
		months := []string{"Januari", "Februari", "Maret", "April", "Mei", "Juni", "Juli", "Agustus", "September", "Oktober", "November", "Desember"}
		period := months[start.In(wib).Month()-1] + " " + fmt.Sprint(start.In(wib).Year())
		visibility := "full"
		if typ == "sealed" {
			visibility = "sealed"
		}
		type lot struct {
			it      demoItem
			qty     float64
			reserve int64
			auction *demoAuction
			id      string
		}
		var ls []*lot
		var value float64
		for i := range lots {
			it := items[(k+i)%len(items)]
			l := &lot{it: it, qty: niceQty(g.f(it.QtyMin, it.QtyMax) * 2), id: newID()}
			l.reserve = nicePrice(float64(it.PriceMin+it.PriceMax) / 2 * map[bool]float64{true: 0.95, false: 1.04}[objective == "selling"])
			value += l.qty * float64(l.reserve)
			ls = append(ls, l)
		}
		title := fmt.Sprintf("Pengadaan %s %s", ls[0].it.Name, period)
		if objective == "selling" {
			title = fmt.Sprintf("Penjualan %s %s", ls[0].it.Name, period)
		}
		if lots > 1 {
			title = fmt.Sprintf("Pengadaan %s %s", strings.ToLower(categoryLabels[ls[0].it.Category]), period)
		}
		required := requiredApprovers(int64(value), "auction", demoRules)
		var procurement any
		if objective == "procurement" && status != "pending_approval" {
			pst := map[string]string{"awarded": "po_issued", "live": "in_auction", "closed": "in_auction", "scheduled": "in_auction"}[status]
			procurement = g.procurement(o, ls[0].it, ls[0].qty, pst, nil, created.Add(-2*day))
		}
		stored := map[string]string{"awarded": "awarded", "live": "live", "closed": "live", "scheduled": "scheduled", "pending_approval": "pending_approval"}[status]
		var invited []string
		for _, s := range sellers {
			if slices.Contains(s.Def.Supply, ls[0].it.Category) && s != o && len(invited) < 3 {
				invited = append(invited, s.SupplierID)
			}
		}
		g.ins("org_auctions", "id, org_id, code, title, category_id, type, objective, min_step_idr, bid_visibility, auto_extension, withdraw_rule, award_rule, "+
			"qual_documents, qual_min_rating, qual_regions, starts_at, duration_minutes, procurement_request_id, status, value_idr, required_approvers, created_by, created_at, updated_at",
			id, o.ID, code, title, ls[0].it.Category, typ, objective, niceStep(float64(ls[0].reserve)*0.005), visibility, true, "before_last_30", "lowest",
			[]string{"NIB", "NPWP"}, 3.5, []string{}, start, duration, procurement, stored, int64(value), required, by.ID, created, created)
		for _, s := range invited {
			g.ins("org_auction_invites", "org_auction_id, supplier_id", id, s)
		}
		for i, role := range required {
			if status == "pending_approval" && i > 0 {
				break
			}
			m := o.member(role)
			g.ins("org_auction_approvals", "org_auction_id, role, decision, decided_by, decided_at, on_behalf", id, role, "approved", m.ID,
				created.Add(hrs(float64(i+1)*g.f(1, 6))), m.Role != role)
		}
		var supplierNames []string
		for _, s := range invited {
			for _, x := range sellers {
				if x.SupplierID == s {
					supplierNames = append(supplierNames, x.Name)
				}
			}
		}
		for i, l := range ls {
			if status != "pending_approval" {
				ltitle := title
				if lots > 1 {
					ltitle = fmt.Sprintf("%s · Lot %d: %s", title, i+1, l.it.Name)
				}
				astatus := map[string]string{"awarded": "awarded", "live": "live", "closed": "closed", "scheduled": "scheduled"}[status]
				a := &demoAuction{Title: ltitle, Category: l.it.Category, Type: typ, Status: astatus, Visibility: visibility, Item: l.it.Name,
					Spec: demoPick(g, l.it.Specs), Unit: l.it.Unit, Region: regionOf(o.Def.City), OwnerOrg: o, Qty: l.qty, Opening: l.reserve,
					Step: niceStep(float64(l.reserve) * 0.005), Start: start, End: start.Add(time.Duration(duration) * time.Minute), Created: created,
					CreatedBy: by.ID, Ext: true, Invitees: supplierNames}
				a.Rules = g.orgLotRules(o, typ, objective, l.reserve, l.it.Unit, a.Step, i, lots)
				if astatus != "scheduled" {
					pool := g.pickParties(g.partiesFor(l.it.Category, map[bool]string{true: "buy", false: "supply"}[objective == "selling"]), g.i(3, 6), o.Party)
					target := float64(l.reserve) * g.f(0.88, 0.96)
					if objective == "selling" {
						target = float64(l.reserve) * g.f(1.04, 1.12)
					}
					g.ladder(a, pool, g.i(4, 12), minTime(a.End, g.now.Add(-10*time.Minute)), target)
				}
				var winners map[*demoParty]bool
				if astatus == "awarded" && len(a.Bids) > 0 {
					winners = map[*demoParty]bool{a.offers()[0].Party: true}
				} else if astatus == "awarded" {
					a.Status = "closed"
				}
				a.finishBids(winners)
				g.writeAuction(a)
				l.auction = a
			}
			var aid any
			if l.auction != nil {
				aid = l.auction.ID
			}
			g.ins("org_auction_lots", "id, org_auction_id, position, item, quantity, unit, spec, reserve_price_idr, auction_id, created_at, updated_at",
				l.id, id, i+1, l.it.Name, l.qty, l.it.Unit, demoPick(g, l.it.Specs), l.reserve, aid, created, created)
		}
		if status != "awarded" {
			continue
		}
		awardAt := start.Add(time.Duration(duration)*time.Minute + hrs(g.f(2, 30)))
		if awardAt.After(g.now) {
			awardAt = g.now.Add(-time.Hour)
		}
		award := newID()
		g.ins("org_awards", "id, org_auction_id, reason, awarded_by, awarded_at", award, id, "Harga terbaik dengan skor supplier memenuhi syarat.", o.Owner.ID, awardAt)
		po := fmt.Sprintf("PO-%s-%04d", poInitials(o.Name), 1000+k)
		g.ins("purchase_orders", "org_id, org_award_id, po_number, issued_by, issued_at", o.ID, award, po, o.Owner.ID, awardAt.Add(time.Hour))
		for i, l := range ls {
			if l.auction == nil || len(l.auction.Bids) == 0 {
				continue
			}
			os := l.auction.offers()
			for _, b := range os {
				var sup any
				if b.Party.OrgID != nil {
					for _, x := range sellers {
						if x.Party == b.Party {
							sup = x.SupplierID
						}
					}
				}
				g.ins("org_auction_offers", "id, org_auction_lot_id, party_id, supplier_id, price_idr, capacity, submitted_at, created_at", b.ID, l.id, b.Party.ID,
					sup, b.Price, l.qty, b.At, awardAt)
			}
			w := os[0]
			t := &demoTrade{Title: fmt.Sprintf("%s · %s %s", l.it.Name, qtyLabel(l.qty), l.it.Unit), Buyer: o.Party, Supplier: w.Party, Qty: l.qty,
				Unit: l.it.Unit, Price: w.Price, AuctionID: &l.auction.ID, Address: o.Def.City, Start: awardAt.Add(time.Duration(i+1) * time.Hour),
				Via: "auction", Category: l.it.Category, Region: regionOf(o.Def.City), Item: l.it.Name, BuyerOrgID: &o.ID, Budget: l.reserve, MarketUnit: l.reserve}
			if objective == "selling" {
				t.Buyer, t.Supplier, t.BuyerOrgID, t.SupplierOrgID = w.Party, o.Party, nil, &o.ID
			}
			if t.Start.After(g.now) {
				t.Start = g.now.Add(-30 * time.Minute)
			}
			g.trade(t)
			g.ins("org_award_lines", "org_award_id, org_auction_lot_id, position, org_auction_offer_id, quantity, price_idr, trade_id, created_at, updated_at",
				award, l.id, 0, w.ID, l.qty, w.Price, t.ID, awardAt, awardAt)
		}
	}
}

func (g *demoGen) orgLotRules(o *demoOrg, typ, objective string, reserve int64, unit string, step int64, i, lots int) []api.LabeledValue {
	typeLabel := auctionTypeLabel[typ] + " auction"
	if lots > 1 {
		typeLabel += fmt.Sprintf(" · lot %d dari %d", i+1, lots)
	}
	res, stepL := "Harga target", "Penurunan minimum"
	if typ == "forward" {
		res, stepL = "Reserve", "Kenaikan minimum"
	}
	return []api.LabeledValue{{Label: "Tipe", Value: typeLabel}, {Label: "Penyelenggara", Value: o.Name},
		{Label: res, Value: fmt.Sprintf("%s per %s", rupiah(reserve), unit)}, {Label: stepL, Value: rupiah(step)},
		{Label: "Perpanjangan otomatis", Value: "+5 menit jika ada bid di 2 menit terakhir"}, {Label: "Kualifikasi", Value: "Rating ≥ 3.5, dokumen: NIB, NPWP"},
		{Label: "Penarikan bid", Value: withdrawRuleLabel["before_last_30"]}, {Label: "Penetapan pemenang", Value: awardRuleLabel("lowest", typ == "forward")}}
}

func (g *demoGen) seedTrades() {
	var supply []*demoListing
	for _, l := range g.listings {
		if l.Kind == "supply" && l.Status != "expired" {
			supply = append(supply, l)
		}
	}
	if len(supply) == 0 {
		return
	}
	var inMarket []*demoListing
	for _, l := range supply {
		if l.MarketID != nil {
			inMarket = append(inMarket, l)
		}
	}
	for range g.sc.DirectTrades {
		l := demoPick(g, supply)
		if len(inMarket) > 0 && g.chance(0.35) {
			l = demoPick(g, inMarket)
		}
		buyer := g.pickParties(g.partiesFor(l.Category, "buy"), 1, l.Owner)[0]
		var regulars []*demoParty
		for _, t := range g.trades {
			if t.Supplier == l.Owner && t.Buyer != l.Owner {
				regulars = append(regulars, t.Buyer)
			}
		}
		if len(regulars) > 0 && g.chance(0.45) {
			buyer = demoPick(g, regulars)
		}
		start := l.Created.Add(hrs(g.f(2, math.Max(3, g.now.Sub(l.Created).Hours()*0.9))))
		if w := g.workHours(start); w.Before(g.now) {
			start = w
		}
		qty := niceQty(g.f(l.It.QtyMin, l.It.QtyMax) * g.f(0.2, 0.6))
		t := &demoTrade{Title: fmt.Sprintf("%s · %s %s", l.Item, qtyLabel(qty), l.Unit), Buyer: buyer, Supplier: l.Owner, Qty: qty, Unit: l.Unit,
			Price: l.Price, Terms: []string{"escrow", "escrow", "escrow", "net14", "net30"}[g.r.IntN(5)], ListingID: &l.ID, Address: buyer.Location,
			Start: start, Via: "direct", Category: l.Category, Region: l.Location}
		if l.MarketID != nil {
			for _, m := range g.markets {
				if m.ID == *l.MarketID {
					t.MarketID, t.Maker = &m.ID, m.Maker.Party
				}
			}
		}
		g.trade(t)
	}

	n := 0
	for _, t := range g.trades {
		if n >= g.sc.RFQs/2 || t.Buyer.Kind != "user" || t.Supplier.Kind != "user" || t.Status == "cancelled" {
			continue
		}
		n++
		g.conversation(t.Title, "transaction", t.ID, []*demoParty{t.Buyer, t.Supplier}, t.Start.Add(time.Hour), minTime(t.Start.Add(hrs(30)), g.now.Add(-time.Minute)),
			g.tradeChat(t.Buyer.Actor, t.Supplier.Actor, strings.Split(t.Title, " · ")[0], t.Unit, t.Qty))
	}
}

func niceStep(v float64) int64 {
	for _, s := range []int64{1, 5, 10, 25, 50, 100, 250, 500, 1_000, 2_500, 5_000, 10_000, 25_000, 50_000, 100_000, 250_000} {
		if float64(s) >= v*0.7 {
			return s
		}
	}
	return 500_000
}

package server

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	cryptorand "crypto/rand"

	"github.com/azdharsyahputra/ecopurnity-api/internal/auth"
	"github.com/azdharsyahputra/ecopurnity-api/internal/secure"
)

type demoParty struct {
	ID, Name, Kind, Display string
	Location, Region        string
	UserID, OrgID           *string
	Actor                   *demoPerson
	Supply, Buy             []string
	Q                       float64
	Size                    float64
	Verified                bool
}

func (p *demoParty) actorID() *string { return &p.Actor.ID }

type demoPerson struct {
	ID, Name, Username, Email, Location string
	Created                             time.Time
	Party                               *demoParty
	Org                                 *demoOrg
	Role                                string
	MM                                  bool
	Verified, EmailVerified, Female     bool
	Cats                                []string
}

type demoOrg struct {
	ID, Name, Slug, SupplierID string
	Def                        demoOrgDef
	Party                      *demoParty
	Owner                      *demoPerson
	Members                    []*demoPerson
	Created                    time.Time
}

func (o *demoOrg) member(role string) *demoPerson {
	for _, m := range o.Members {
		if m.Role == role {
			return m
		}
	}
	return o.Owner
}

func (g *demoGen) run() error {
	steps := []struct {
		name string
		fn   func()
	}{
		{"people", g.seedPeople}, {"organizations", g.seedOrgs}, {"markets", g.seedMarkets}, {"listings", g.planListings},
		{"auctions", g.seedAuctions}, {"listings", g.writeListings}, {"trades", g.seedTrades}, {"rfqs", g.seedRFQs},
		{"contracts", g.seedContracts}, {"pools", g.seedPoolsAndProcurement}, {"purchase history", g.seedPurchaseHistory},
		{"payouts", g.seedWithdrawals}, {"market stats", g.marketStats},
	}
	for _, st := range steps {
		t := time.Now()
		st.fn()
		g.flush()
		if g.err != nil {
			return fmt.Errorf("%s: %w", st.name, g.err)
		}
		g.logf("  %-16s %6.1fs", st.name, time.Since(t).Seconds())
	}
	t := time.Now()
	if err := g.detectOpportunities(); err != nil {
		return fmt.Errorf("opportunities: %w", err)
	}
	g.logf("  %-16s %6.1fs", "opportunities", time.Since(t).Seconds())
	return nil
}

func (g *demoGen) hashPasswords(n int) error {
	g.hashes = make([]string, n)
	var wg sync.WaitGroup
	errs := make([]error, n)
	sem := make(chan struct{}, 8)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			g.hashes[i], errs[i] = auth.HashPassword(cryptorand.Text() + cryptorand.Text())
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (g *demoGen) provinceOf(city string) string {
	for prov, cities := range demoProvinceCities {
		for _, c := range cities {
			if c == city {
				return prov
			}
		}
	}
	return regionOf(city)
}

func (g *demoGen) newPerson(city string) *demoPerson {
	prov := g.provinceOf(city)
	var n demoName
	for {
		n = g.personName(prov)
		if !g.usedNames[n.Full] {
			break
		}
	}
	g.usedNames[n.Full] = true
	u := g.username(n, city)
	for k := 2; g.takenUsers[u]; k++ {
		u = fmt.Sprintf("%s%d", strings.TrimRight(u, "0123456789"), g.i(10, 99))
	}
	g.takenUsers[u] = true
	local := asciiLower(n.First) + "." + asciiLower(n.Last)
	for k := 2; g.takenEmails[local]; k++ {
		local = fmt.Sprintf("%s.%s%d", asciiLower(n.First), asciiLower(n.Last), k)
	}
	g.takenEmails[local] = true
	created := g.ago(float64(g.days) + g.f(-25, 200))
	for wd := created.In(wib).Weekday(); (wd == time.Saturday || wd == time.Sunday) && g.chance(0.6); wd = created.In(wib).Weekday() {
		created = created.Add(-24 * time.Hour)
	}
	p := &demoPerson{ID: newID(), Name: n.Full, Username: u, Email: local + "@" + DemoEmailDomain, Location: city, Female: n.Female,
		Created: g.workHours(created), EmailVerified: g.chance(0.92), Verified: g.chance(0.27)}
	p.EmailVerified = p.EmailVerified || p.Verified
	g.people = append(g.people, p)
	return p
}

func (g *demoGen) seedPeople() {
	g.usedNames, g.takenUsers, g.takenEmails = map[string]bool{}, map[string]bool{}, map[string]bool{}
	rows, err := g.tx.Query(g.ctx, `SELECT username::text FROM users`)
	if err == nil {
		var names []string
		names, err = collectStrings(rows)
		for _, n := range names {
			g.takenUsers[n] = true
		}
	}
	if err != nil {
		g.err = err
	}
}

func (g *demoGen) writePeople() {
	for i, p := range g.people {
		var verifiedEmail any
		if p.EmailVerified || p.Org != nil {
			verifiedEmail = p.Created.Add(time.Duration(g.i(2, 40)) * time.Minute)
		}
		g.ins("users", "id, name, username, email, email_verified_at, password_hash, location, onboarded_at, created_at, updated_at",
			p.ID, p.Name, p.Username, p.Email, verifiedEmail, g.hashes[i], p.Location, p.Created.Add(15*time.Minute), p.Created, p.Created)
		var verifiedAt, verifiedBy any
		if p.Verified {
			verifiedAt, verifiedBy = g.workHours(p.Created.Add(hrs(g.f(24, 24*20)))), p.ID
		}
		cats := p.Cats
		if len(cats) == 0 {
			cats = []string{"food"}
		}
		g.ins("identities", "user_id, bio, pref_locations, pref_categories, delivery_radius_km, identity_verified_at, identity_verified_by, created_at, updated_at",
			p.ID, g.bio(cats[0]), []string{p.Location}, "{"+strings.Join(cats, ",")+"}", g.i(2, 10)*10, verifiedAt, verifiedBy, p.Created, p.Created)
		if g.chance(0.4) {
			for k, it := range itemsIn(cats[0])[:min(2, len(itemsIn(cats[0])))] {
				g.ins("capacity_items", "user_id, position, kind, name, detail, category_id, created_at, updated_at", p.ID, k, "capacity", it.Name,
					fmt.Sprintf("%s %s/bulan", idNumber(niceQty(it.QtyMax/4)), it.Unit), it.Category, p.Created, p.Created)
			}
		}
		for _, t := range notificationTypes {
			g.ins("notification_prefs", "user_id, type, in_app, email", p.ID, t, true, false)
		}
	}
}

func (g *demoGen) party(name, kind, location string, userID, orgID *string, actor *demoPerson, verified bool, created time.Time) *demoParty {
	display := "person"
	if kind == "org" {
		display = "business"
	}
	p := &demoParty{ID: newID(), Name: name, Kind: kind, Display: display, Location: location, Region: regionOf(location), UserID: userID,
		OrgID: orgID, Actor: actor, Q: g.f(0.55, 1), Verified: verified, Size: 0.3}
	g.ins("parties", "id, kind, user_id, org_id, name, display_kind, verified, created_at", p.ID, kind, userID, orgID, name, display, verified, created)
	g.parties = append(g.parties, p)
	return p
}

var demoOrgSize = map[string]float64{"UMKM": 0.5, "Kelompok tani": 0.8, "Koperasi": 1, "Asosiasi": 1, "CV": 1.2, "PT": 1.8}

func (g *demoGen) seedOrgs() {
	defs := append(append([]demoOrgDef{}, demoMMOrgs[:g.sc.MMs]...), demoBizOrgs[:g.sc.Orgs]...)
	roles := []string{"procurement", "finance", "operations", "sales"}
	staff := 0
	for i, d := range defs {
		owner := g.newPerson(d.City)
		owner.Created = g.ago(float64(g.days) + g.f(60, 220))
		o := &demoOrg{ID: newID(), Name: d.Name, Slug: slugify(d.Name), Def: d, Owner: owner, Created: g.workHours(owner.Created.Add(hrs(g.f(1, 72))))}
		owner.Org, owner.Role, owner.MM, owner.Cats = o, "owner", d.MM, d.Supply
		o.Members = []*demoPerson{owner}
		if i%3 != 2 && staff < g.sc.People/3 {
			for k := range g.i(1, 3) {
				cities := demoProvinceCities[g.provinceOf(d.City)]
				city := d.City
				if g.chance(0.4) {
					city = demoPick(g, cities)
				}
				m := g.newPerson(city)
				m.Created = o.Created.Add(hrs(g.f(24, 24*120)))
				m.Org, m.Role, m.Cats = o, roles[k%len(roles)], d.Buy
				o.Members = append(o.Members, m)
				staff++
			}
		}
		if d.MM {
			g.mmOrgs = append(g.mmOrgs, o)
		} else {
			g.orgs = append(g.orgs, o)
		}
	}
	for len(g.people) < g.sc.People {
		prov := demoWeighted(g, demoProvinceWeight)
		p := g.newPerson(demoPick(g, demoProvinceCities[prov]))
		var cats []string
		for _, it := range demoItems {
			if slices.ContainsFunc(it.Cities, func(c string) bool { return regionOf(c) == regionOf(p.Location) }) {
				cats = append(cats, it.Category)
			}
		}
		if len(cats) == 0 {
			cats = []string{demoItems[g.r.IntN(len(demoItems))].Category}
		}
		p.Cats = []string{demoPick(g, cats)}
	}
	g.staff = g.newPerson("Jakarta Selatan, DKI Jakarta")
	g.staff.Created, g.staff.EmailVerified, g.staff.Verified = g.ago(float64(g.days)+300), true, true
	if err := g.hashPasswords(len(g.people)); err != nil {
		g.err = err
		return
	}
	g.writePeople()
	for _, o := range append(append([]*demoOrg{}, g.mmOrgs...), g.orgs...) {
		g.writeOrg(o)
	}
	buyCats := []string{"food", "packaging", "agri", "logistics", "it"}
	for _, p := range g.people {
		p.Party = g.party(p.Name, "user", p.Location, &p.ID, nil, p, p.EmailVerified || p.Org != nil, p.Created.Add(time.Hour))
		if p.Org != nil {
			p.Party.Supply, p.Party.Buy = p.Org.Def.Supply[:1], p.Org.Def.Buy[:1]
		} else if len(p.Cats) > 0 {
			p.Party.Supply, p.Party.Buy = p.Cats, []string{demoPick(g, buyCats)}
		}
	}
	for _, o := range g.mmOrgs {
		g.ins("user_capabilities", "user_id, capability, granted_at", o.Owner.ID, "market_maker", o.Created.Add(72*time.Hour))
	}
	g.flush()
	g.makeAccounts()
}

func (g *demoGen) writeOrg(o *demoOrg) {
	d := o.Def
	g.ins("orgs", "id, name, slug, created_by, created_at, updated_at", o.ID, o.Name, o.Slug, o.Owner.ID, o.Created, o.Created)
	nib := fmt.Sprintf("%013d", g.r.Int64N(9_000_000_000_000)+1_000_000_000_000)
	npwp := fmt.Sprintf("%02d.%03d.%03d.%d-%03d.000", g.i(1, 99), g.i(0, 999), g.i(0, 999), g.i(0, 9), g.i(0, 999))
	region := regionOf(d.City)
	g.ins("org_profiles", "org_id, org_type, industry, location, region, description, categories, nib, npwp, akta, verification, created_at, updated_at",
		o.ID, d.Type, d.Industry, d.City, region, d.Desc, "{"+strings.Join(demoUniq(append(append([]string{}, d.Supply...), d.Buy...)), ",")+"}",
		nib, npwp, "Akta No. "+fmt.Sprint(g.i(1, 99))+" tanggal "+o.Created.AddDate(-g.i(2, 12), 0, 0).Format("2 Jan 2006"), d.Verification, o.Created, o.Created)
	for i, r := range builtInRoles {
		g.ins("org_roles", "org_id, key, label, position, permissions", o.ID, r.Key, r.Label, i, r.Permissions)
	}
	g.ins("org_settings", "org_id, savings_target_idr, service_regions", o.ID, int64(g.i(5, 40))*1_000_000, []string{region})
	g.ins("org_approval_rules", "org_id, position, label, min_amount_idr, approvers, applies_to", o.ID, 0, "Procurement > Rp 50 jt", 50_000_000,
		[]string{"finance", "owner"}, []string{"procurement", "auction"})
	g.ins("org_approval_rules", "org_id, position, label, min_amount_idr, approvers, applies_to", o.ID, 1, "Auction > Rp 200 jt", 200_000_000,
		[]string{"owner", "procurement"}, []string{"auction"})
	for k, m := range o.Members {
		joined := o.Created.Add(hrs(float64(k) * g.f(2, 200)))
		g.ins("org_members", "org_id, user_id, email, name, role, department, status, joined_at, created_at", o.ID, m.ID, m.Email, m.Name, m.Role,
			map[string]string{"owner": "Direksi", "procurement": "Pengadaan", "finance": "Keuangan", "operations": "Operasional", "sales": "Penjualan"}[m.Role],
			"active", joined, joined)
	}
	g.ins("org_warehouses", "org_id, name, location, capacity_m2", o.ID, "Gudang utama", d.City, g.i(2, 30)*50)
	if d.Verification == "verified" || d.Verification == "pending" {
		for _, kind := range []string{"nib", "npwp", "akta"} {
			g.ins("org_documents", "org_id, kind, name, uploaded_by, uploaded_at", o.ID, kind, strings.ToUpper(kind)+"-"+o.Slug+".pdf", o.Owner.ID, o.Created.Add(24*time.Hour))
		}
	}
	if d.Verification == "pending" {
		form, _ := json.Marshal([]map[string]string{{"label": "Nama usaha", "value": o.Name}, {"label": "Jenis", "value": d.Type}, {"label": "NIB", "value": nib}})
		g.ins("verification_requests", "kind, org_id, submitted_by, subject_name, form, created_at, updated_at", "business", o.ID, o.Owner.ID, o.Name, form,
			g.ago(g.f(0.5, 3)), g.now)
	}
	o.Party = g.party(o.Name, "org", d.City, nil, &o.ID, o.Owner, d.Verification == "verified", o.Created)
	o.Party.Supply, o.Party.Buy, o.Party.Size = d.Supply, d.Buy, demoOrgSize[d.Type]

	if len(d.Supply) > 0 && !d.MM {
		o.SupplierID = newID()
		g.ins("suppliers", "id, name, categories, region, seed_rating, verified, documents, capacity, party_id, created_at", o.SupplierID, o.Name,
			"{"+strings.Join(d.Supply, ",")+"}", region, math.Round(g.f(3.6, 4.8)*10)/10, d.Verification == "verified",
			[]string{"NIB.pdf", "NPWP.pdf"}, d.Industry, o.Party.ID, o.Created)
		for k := range 6 {
			month := time.Date(g.now.Year(), g.now.Month()-time.Month(5-k), 1, 0, 0, 0, 0, time.UTC)
			base := 70 + 25*o.Party.Q
			sc := func(d float64) float64 { return math.Max(40, math.Min(99, math.Round(base+d+g.f(-6, 6)))) }
			g.ins("supplier_scorecards", "supplier_id, month, price, reliability, quality, delivery", o.SupplierID, month, sc(0), sc(-2), sc(3), sc(-4))
		}
	}
}

func demoUniq(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func (g *demoGen) makeAccounts() {
	if g.err != nil {
		return
	}
	for _, k := range []string{"platform_revenue", "bank_clearing", "payout_pending"} {
		id, err := ledgerAccount(g.ctx, g.tx, "", k)
		if err != nil {
			g.err = err
			return
		}
		g.acct[k+":"] = id
	}
	for _, p := range g.parties {
		kinds := []string{"wallet_available", "escrow", "ppn_payable"}
		if p.Kind == "org" {
			kinds = append(kinds, "maker_commission")
		}
		for _, k := range kinds {
			id := newID()
			g.acct[k+":"+p.ID] = id
			g.ins("ledger_accounts", "id, owner_party_id, kind", id, p.ID, k)
		}
	}
}

type demoMarket struct {
	ID, Code  string
	Def       demoMarketDef
	Item      demoItem
	Maker     *demoOrg
	Op        *demoPerson
	Created   time.Time
	Buyers    []*demoParty
	Suppliers []*demoParty
	Versions  []demoRuleVersion
	Ref       float64
	Type      string
}

type demoRuleVersion struct {
	ID        string
	From      int
	Rules     marketRules
	CreatedAt time.Time
}

func demoItemByName(name string) demoItem {
	for _, it := range demoItems {
		if it.Name == name {
			return it
		}
	}
	panic("demo item " + name)
}

func (g *demoGen) partiesFor(cat, side string) []*demoParty {
	var out []*demoParty
	for _, p := range g.parties {
		cats := p.Supply
		if side == "buy" {
			cats = p.Buy
		}
		if slices.Contains(cats, cat) && !(p.Kind == "org" && slices.ContainsFunc(g.mmOrgs, func(o *demoOrg) bool { return o.Party == p })) {
			out = append(out, p)
		}
	}
	return out
}

func (g *demoGen) pickParties(prefer []*demoParty, n int, not ...*demoParty) []*demoParty {
	var out []*demoParty
	add := func(pool []*demoParty) {
		for _, k := range g.r.Perm(len(pool)) {
			p := pool[k]
			if len(out) < n && !slices.Contains(out, p) && !slices.Contains(not, p) {
				out = append(out, p)
			}
		}
	}
	add(prefer)
	add(g.parties)
	return out
}

func (g *demoGen) seedMarkets() {
	for i, d := range demoMarkets {
		if len(g.markets) == g.sc.Markets || d.Maker >= len(g.mmOrgs) {
			continue
		}
		maker := g.mmOrgs[d.Maker]
		it := demoItemByName(d.Item)
		m := &demoMarket{ID: newID(), Code: g.code("MKT"), Def: d, Item: it, Maker: maker, Op: maker.Owner, Ref: d.Ref, Type: roundType[d.Mech],
			Created: g.ago(float64(g.days) + g.f(-10, 40) - float64(i)*3)}
		if d.Status == "formation" {
			m.Created = g.ago(g.f(4, 9))
		}
		g.markets = append(g.markets, m)
		priceMin, priceMax := nicePrice(d.Ref*0.95), nicePrice(d.Ref*1.05)
		desc := fmt.Sprintf("%s dibentuk oleh %s. %s Objective %s dengan mekanisme %s.", d.Name, maker.Name, d.Desc,
			strings.ToLower(objectiveLabel[d.Objective]), strings.ToLower(mechanismLabel[d.Mech]))
		g.ins("markets", "id, code, name, category_id, region, objective, mechanism, status, maker_party_id, description, unit, demand_value, supply_value, "+
			"price_min_idr, price_max_idr, created_by, created_at, updated_at",
			m.ID, m.Code, d.Name, it.Category, d.Region, d.Objective, d.Mech, d.Status, maker.Party.ID, desc, it.Unit, d.Demand, d.Supply,
			priceMin, priceMax, m.Op.ID, m.Created, m.Created)
		g.ins("market_operators", "market_id, user_id, added_by, created_at", m.ID, m.Op.ID, m.Op.ID, m.Created)
		if co := maker.member("procurement"); co != m.Op {
			g.ins("market_operators", "market_id, user_id, added_by, created_at", m.ID, co.ID, m.Op.ID, m.Created.Add(48*time.Hour))
		}
		approval := "manual"
		if d.Eligibility == "open" {
			approval = "auto"
		}
		g.ins("market_settings", "market_id, approval, supplier_verification, updated_by, created_at, updated_at", m.ID, approval, "documents", m.Op.ID, m.Created, m.Created)

		rules := defaultRules(d.Demand, d.Supply, d.Region, d.Mech, m.Created)
		rules.Eligibility = d.Eligibility
		rules.MinStepPct = 0.5
		rules.WindowEnd = g.now.AddDate(0, 3, 0).Format(time.DateOnly)
		m.Versions = []demoRuleVersion{{ID: newID(), From: 1, Rules: rules, CreatedAt: m.Created}}
		if d.Status != "formation" && i%3 != 2 {
			r2 := rules
			r2.MinStepPct, r2.MinQuantity = 0.75, math.Round(rules.MinQuantity*1.5)
			m.Versions = append(m.Versions, demoRuleVersion{ID: newID(), From: g.i(3, 5), Rules: r2, CreatedAt: g.ago(float64(g.days) * g.f(0.4, 0.7))})
		}
		author := m.Op.Name + " (Market Maker)"
		for k, v := range m.Versions {
			raw, _ := json.Marshal(v.Rules)
			var reason any
			if k > 0 {
				reason = "Langkah bid terlalu kecil, round berjalan lambat"
			}
			g.ins("market_rule_versions", "id, market_id, version, rules, effective_from_round, author, created_by, reason, created_at",
				v.ID, m.ID, k+1, raw, v.From, author, m.Op.ID, reason, v.CreatedAt)
		}
		g.activity("market_formed", "Market terbentuk: "+d.Name, nil, &m.ID, m.Created)

		buyers := g.pickParties(g.partiesFor(it.Category, "buy"), g.i(8, 14), maker.Party)
		suppliers := g.pickParties(g.partiesFor(it.Category, "supply"), g.i(8, 14), maker.Party)
		for role, list := range map[string][]*demoParty{"buyer": buyers, "supplier": suppliers} {
			for k, p := range list {
				if role == "buyer" && slices.Contains(suppliers, p) {
					continue
				}
				status, note := "active", any(nil)
				switch {
				case d.Status == "formation" && k%2 == 0, k == len(list)-1:
					status = "pending"
				case k == len(list)-2 && role == "supplier":
					status, note = "suspended", "Gagal kirim dua round berturut-turut"
				}
				joined := m.Created.Add(hrs(g.f(2, 24*20)))
				if status == "pending" {
					joined = g.ago(g.f(0.2, 4))
				}
				g.ins("market_participants", "market_id, party_id, role, status, verified, note, joined_at, created_at, updated_at",
					m.ID, p.ID, role, status, role == "supplier" && p.Verified, note, joined, joined, joined)
				if status == "active" {
					if role == "buyer" {
						m.Buyers = append(m.Buyers, p)
					} else {
						m.Suppliers = append(m.Suppliers, p)
					}
				}
				if p.UserID != nil {
					g.ins("watchlist", "user_id, market_id, joined_at, created_at, updated_at", *p.UserID, m.ID, joined, joined, joined)
				}
			}
		}

		for k := range g.i(0, 2) {
			if len(m.Buyers) == 0 || len(m.Suppliers) == 0 {
				break
			}
			st := []string{"open", "review", "resolved"}[k%3]
			var res any
			if st == "resolved" {
				res = "Supplier mengganti barang yang kurang dalam 3 hari; kasus ditutup."
			}
			opened := g.ago(g.f(1, 20))
			g.ins("market_disputes", "market_id, title, parties, status, resolution, opened_at, created_at, updated_at", m.ID, g.disputeReason(it.Category),
				demoPick(g, m.Buyers).Name+" vs "+demoPick(g, m.Suppliers).Name, st, res, opened, opened, opened)
		}
	}
}

func (m *demoMarket) version(round int) demoRuleVersion {
	v := m.Versions[0]
	for _, x := range m.Versions {
		if x.From <= round {
			v = x
		}
	}
	return v
}

func (g *demoGen) marketStats() {
	for _, m := range g.markets {
		var vol int64
		lo, hi := int64(math.MaxInt64), int64(0)
		for _, t := range g.trades {
			if t.MarketID == nil || *t.MarketID != m.ID || t.Start.Before(g.ago(30)) {
				continue
			}
			vol += t.Total
			lo, hi = min(lo, t.Price), max(hi, t.Price)
		}
		if hi == 0 {
			continue
		}
		g.q(`UPDATE markets SET volume_30d_idr = $2, price_min_idr = $3, price_max_idr = $4 WHERE id = $1`, m.ID, vol, lo, hi)
	}
}

type demoListing struct {
	ID, Code, Kind, Status, Item, Category, Unit, Location, Spec, Delivery string
	Owner                                                                  *demoParty
	Qty                                                                    float64
	Price, Budget                                                          int64
	Created, Updated                                                       time.Time
	AvailableFrom, Expires, Deadline                                       *time.Time
	MarketID, AuctionID                                                    *string
	Events                                                                 [][2]any
	It                                                                     demoItem
}

func (g *demoGen) planListings() {
	for range g.sc.Listings {
		it := demoPick(g, demoItems)
		kind := "supply"
		if g.chance(it.DemandBias) {
			kind = "demand"
		}
		side := map[string]string{"supply": "supply", "demand": "buy"}[kind]
		owners := g.partiesFor(it.Category, side)

		var near []*demoParty
		for _, p := range owners {
			if slices.ContainsFunc(it.Cities, func(c string) bool { return regionOf(c) == p.Region }) {
				near = append(near, p)
			}
		}
		if len(near) > 0 && g.chance(0.8) {
			owners = near
		}
		if len(owners) == 0 {
			owners = g.parties
		}
		owner := demoPick(g, owners)
		loc := demoPick(g, it.Cities)
		for _, c := range g.r.Perm(len(it.Cities)) {
			if regionOf(it.Cities[c]) == owner.Region {
				loc = it.Cities[c]
			}
		}
		if kind == "demand" {
			loc = owner.Location
		}
		l := &demoListing{ID: newID(), Kind: kind, Item: it.Name, Category: it.Category, Unit: it.Unit, Location: loc, Spec: demoPick(g, it.Specs),
			Delivery: it.Delivery, Owner: owner, Qty: niceQty(math.Max(it.QtyMin*0.5, g.f(it.QtyMin, it.QtyMax)*owner.Size*g.f(0.6, 1.4))), It: it}
		price := g.f(float64(it.PriceMin), float64(it.PriceMax))

		old := g.chance(0.2)
		if old {
			l.Created = g.ago(float64(g.days) + g.f(1, 90))
		} else {
			l.Created = g.when()
		}
		age := g.now.Sub(l.Created).Hours() / 24
		l.Updated = l.Created
		if kind == "supply" {
			l.Code, l.Price = g.code("SUP"), nicePrice(price)
			af := l.Created.Add(hrs(g.f(0, 72)))
			exp := af.AddDate(0, 0, g.i(30, 120))
			l.AvailableFrom, l.Expires = &af, &exp
			switch {
			case exp.Before(g.now):
				l.Status = "expired"
			case old || g.chance(0.12):
				l.Status = "sold"
			case g.chance(0.05):
				l.Status = "reserved"
			default:
				l.Status = "available"
			}
		} else {
			l.Code, l.Budget = g.code("DEM"), nicePrice(price*l.Qty*g.f(0.86, 1.0))
			dl := l.Created.AddDate(0, 0, g.i(14, 60))
			l.Deadline = &dl
			switch {
			case dl.Before(g.now) && g.chance(0.6):
				l.Status = "fulfilled"
			case dl.Before(g.now):
				l.Status = []string{"expired", "cancelled"}[g.r.IntN(2)]
			case g.chance(0.1):
				l.Status = "matched"
			default:
				l.Status = "open"
			}
		}

		if l.Status == "available" || l.Status == "open" {
			for _, m := range g.markets {
				if m.Item.Name == it.Name && m.Def.Status != "formation" && regionOf(l.Location) == m.Def.Region && g.chance(0.4) {
					l.Status, l.MarketID = "in_market", &m.ID
					l.Events = append(l.Events, [2]any{"in_market", "Masuk market " + m.Def.Name})
				}
			}
		}
		if l.Status != "available" && l.Status != "open" && l.Status != "in_market" {
			l.Updated = l.Created.Add(hrs(g.f(24, math.Max(25, age*24*0.9))))
			if l.Updated.After(g.now) {
				l.Updated = g.now.Add(-time.Hour)
			}
			l.Events = append(l.Events, [2]any{l.Status, map[string]string{"sold": "Terjual habis", "expired": "Kedaluwarsa", "fulfilled": "Terpenuhi",
				"cancelled": "Dibatalkan pemilik", "matched": "Cocok dengan penawaran", "reserved": "Dipesan pembeli"}[l.Status]})
		}
		if l.Status == "sold" {
			l.Qty = 0
		}
		g.listings = append(g.listings, l)
	}
}

func (g *demoGen) writeListings() {
	for _, l := range g.listings {
		var price, budget any
		if l.Kind == "supply" {
			price = l.Price
		} else {
			budget = l.Budget
		}
		g.ins("listings", "id, code, kind, status, owner_party_id, item, category_id, quantity, unit, location, spec, delivery, market_id, price_idr, "+
			"available_from, expires_at, budget_idr, deadline, auction_id, created_at, updated_at",
			l.ID, l.Code, l.Kind, l.Status, l.Owner.ID, l.Item, l.Category, l.Qty, l.Unit, l.Location, l.Spec, l.Delivery, l.MarketID, price,
			l.AvailableFrom, l.Expires, budget, l.Deadline, l.AuctionID, l.Created, l.Updated)
		initial := "available"
		if l.Kind == "demand" {
			initial = "open"
		}
		g.ins("listing_events", "listing_id, status, note, at", l.ID, initial, "Dibuat", l.Created)
		for k, e := range l.Events {
			at := l.Created.Add(hrs(float64(k+1) * 2))
			if k == len(l.Events)-1 && l.Updated.After(l.Created) {
				at = l.Updated
			}
			g.ins("listing_events", "listing_id, status, note, at", l.ID, e[0], e[1], at)
		}
		qty := l.Qty
		if qty == 0 {
			qty = niceQty(g.f(l.It.QtyMin, l.It.QtyMax))
		}
		value := l.Budget
		if l.Kind == "supply" {
			value = int64(float64(l.Price) * qty)
		}
		g.fact("listing.created", l.ID, l.Created, map[string]any{"kind": l.Kind, "categoryId": l.Category, "region": l.Location, "item": l.Item,
			"quantity": qty, "unit": l.Unit, "valueIdr": value, "partyId": l.Owner.ID})
	}
}

func (g *demoGen) conversation(subject, linkType, linkID string, members []*demoParty, start, last time.Time, lines []string) string {
	id := newID()
	g.ins("conversations", "id, subject, link_type, link_id, created_by, created_at, last_message_at", id, subject, nilIfEmpty(linkType),
		nilIfEmpty(linkID), members[0].actorID(), start, start)
	for _, p := range members {
		g.ins("conversation_participants", "conversation_id, party_id, user_id, joined_at, last_read_seq, last_read_at", id, p.ID, p.Actor.ID, start,
			len(lines), last)
	}
	for k, text := range lines {
		at := start.Add(time.Duration(float64(last.Sub(start)) * float64(k+1) / float64(len(lines))))
		by := members[k%len(members)]
		g.ins("messages", "conversation_id, seq, author_party_id, author_user_id, body, created_at", id, 0, by.ID, by.Actor.ID, text, at)
	}
	return id
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (g *demoGen) seedRFQs() {
	users := g.userParties()
	for k := range g.sc.RFQs {
		it := demoPick(g, demoItems)
		buyer := demoPick(g, users)
		created := g.when()
		qty := niceQty(g.f(it.QtyMin, it.QtyMax))
		target := nicePrice(float64(it.PriceMin+it.PriceMax) / 2 * 0.97)
		id, code := newID(), g.code("RFQ")
		suppliers := g.pickParties(filterUsers(g.partiesFor(it.Category, "supply")), g.i(2, 4), buyer)
		age := g.now.Sub(created)
		status := "open"
		if age > 10*24*time.Hour {
			status = []string{"awarded", "awarded", "closed"}[g.r.IntN(3)]
		} else if age > 3*24*time.Hour && g.chance(0.4) {
			status = "awarded"
		}
		deadline := created.AddDate(0, 0, g.i(7, 21))
		conv := g.conversation("RFQ "+it.Name, "rfq", id, append([]*demoParty{buyer}, suppliers...), created,
			created.Add(time.Duration(float64(min(age, 72*time.Hour))*0.9)), g.rfqChat(buyer.Actor, suppliers[0].Actor, it.Name, it.Unit, buyer.Location, qty, nicePrice(float64(target)*g.f(1.0, 1.08)), demoPick(g, it.Specs)))
		g.ins("rfqs", "id, code, buyer_party_id, item, category_id, quantity, unit, target_price_idr, deadline, location, spec, status, conversation_id, "+
			"created_by, created_at, updated_at", id, code, buyer.ID, it.Name, it.Category, qty, it.Unit, target, deadline, buyer.Location,
			demoPick(g, it.Specs), status, conv, buyer.Actor.ID, created, created)
		for _, s := range suppliers {
			g.ins("rfq_invitations", "rfq_id, party_id, invited_at", id, s.ID, created)
		}
		winner := g.r.IntN(len(suppliers))
		for j, s := range suppliers {
			qid := newID()
			at := created.Add(hrs(g.f(2, 30)))
			price := nicePrice(float64(target) * g.f(0.98, 1.12))
			qst, counter := "submitted", any(nil)
			hist := [][3]any{{s, at, "Penawaran " + unitPrice(price, it.Unit)}}
			switch {
			case status == "awarded" && j == winner:
				qst = "accepted"
				if g.chance(0.5) {
					c := nicePrice(float64(price) * 0.96)
					hist = append(hist, [3]any{buyer, at.Add(hrs(6)), "Tawar balik " + rupiah(c)})
					price = nicePrice(float64(price+c) / 2)
					hist = append(hist, [3]any{s, at.Add(hrs(12)), "Revisi " + rupiah(price)})
				}
			case status != "open":
				qst = "declined"
			case g.chance(0.3):
				qst, counter = "countered", nicePrice(float64(price)*0.95)
				hist = append(hist, [3]any{buyer, at.Add(hrs(5)), "Tawar balik " + rupiah(nicePrice(float64(price)*0.95))})
			}
			terms := []string{"escrow", "escrow", "net14", "net30"}[g.r.IntN(4)]
			g.ins("quotes", "id, rfq_id, supplier_party_id, price_idr, quantity, lead_time_days, terms, note, status, counter_price_idr, created_by, created_at, updated_at",
				qid, id, s.ID, price, qty, g.i(2, 14), terms, demoPick(g, demoQuoteNotes), qst, counter, s.Actor.ID, at, at)
			for _, h := range hist {
				p := h[0].(*demoParty)
				g.ins("quote_events", "quote_id, at, actor_user_id, actor_label, text", qid, h[1], p.Actor.ID, p.Actor.Name, h[2])
			}
			if qst == "accepted" {
				t := &demoTrade{Title: fmt.Sprintf("%s · %s %s", it.Name, qtyLabel(qty), it.Unit), Buyer: buyer, Supplier: s, Qty: qty, Unit: it.Unit,
					Price: price, Terms: terms, QuoteID: &qid, Address: buyer.Location, Start: hist[len(hist)-1][1].(time.Time).Add(hrs(g.f(1, 20))),
					Via: "rfq", Category: it.Category, Region: buyer.Location}
				if t.Start.After(g.now) {
					t.Start = g.now.Add(-time.Hour)
				}
				g.trade(t)
			}
		}
		_ = k
	}
}

func (g *demoGen) userParties() []*demoParty {
	return filterUsers(g.parties)
}

func filterUsers(ps []*demoParty) []*demoParty {
	var out []*demoParty
	for _, p := range ps {
		if p.Kind == "user" {
			out = append(out, p)
		}
	}
	return out
}

func (g *demoGen) seedContracts() {

	var src []*demoTrade
	for _, t := range g.trades {
		if t.Status == "completed" && (t.Via == "direct" || t.Via == "rfq") && t.GroupLabel == nil {
			src = append(src, t)
		}
	}
	for k := 0; k < g.sc.Contracts && k < len(src); k++ {
		t := src[(k*7)%len(src)]
		every := []string{"weekly", "biweekly", "monthly"}[k%3]
		status := []string{"active", "active", "active", "proposed", "paused", "ended"}[k%6]
		created := t.Done.Add(hrs(g.f(2, 48)))
		if created.After(g.now) {
			created = g.now.Add(-2 * time.Hour)
		}
		id, code := newID(), g.code("CTR")
		runs := g.i(4, 12)
		qty := niceQty(t.Qty * g.f(0.5, 1))
		next := created
		var orders []time.Time
		if status != "proposed" {
			for next = nextRun(created, every); next.Before(g.now) && len(orders) < runs-1; next = nextRun(next, every) {
				orders = append(orders, next)
			}
		}
		if status == "ended" {
			runs = max(2, len(orders))
		}
		if !next.After(g.now) {
			next = g.now.Add(24 * time.Hour)
		}
		item := strings.Split(t.Title, " · ")[0]
		g.ins("supply_contracts", "id, code, buyer_party_id, supplier_party_id, item, quantity, unit, unit_price_idr, terms, every, runs, next_at, status, "+
			"proposed_by_side, source_trade_id, created_by, created_at, updated_at", id, code, t.Buyer.ID, t.Supplier.ID, item, qty, t.Unit, t.Price,
			t.Terms, every, runs, next, status, "buyer", t.ID, t.Buyer.Actor.ID, created, created)
		for n, at := range orders {
			o := &demoTrade{Title: fmt.Sprintf("%s · order %d/%d (%s)", item, n+1, runs, code), Buyer: t.Buyer, Supplier: t.Supplier, Qty: qty,
				Unit: t.Unit, Price: t.Price, Terms: t.Terms, Address: t.Address, Start: at, Via: "contract", Category: t.Category, Region: t.Region}
			g.trade(o)
			g.ins("contract_orders", "contract_id, run, trade_id, placed_at", id, n+1, o.ID, at)
		}
	}
}

func (g *demoGen) buyerOrgs(cat string) []*demoOrg {
	var out []*demoOrg
	for _, o := range g.orgs {
		if slices.Contains(o.Def.Buy, cat) {
			out = append(out, o)
		}
	}
	if len(out) == 0 {
		out = g.orgs
	}
	return out
}

func (g *demoGen) seedPoolsAndProcurement() {
	type poolDef struct {
		item, region, status string
	}
	defs := []poolDef{{"Karung plastik 50 kg", "Jawa Tengah", "open"}, {"Box karton double wall 40×30×20 cm", "DKI Jakarta", "market_requested"}}
	for _, d := range defs {
		it := demoItemByName(d.item)
		members := g.buyerOrgs(it.Category)
		if len(members) > 3 {
			members = members[:3]
		}
		price := nicePrice(float64(it.PriceMin+it.PriceMax) / 2)
		ref := niceQty(it.QtyMax / 4)
		created := g.ago(g.f(3, 12))
		id := newID()
		var requested any
		if d.status == "market_requested" {
			requested = g.ago(g.f(0.3, 2))
		}
		g.ins("collective_pools", "id, title, category_id, spec, region, deadline, unit, base_unit_price_idr, ref_qty, threshold_qty, status, market_requested_at, "+
			"created_by, created_at, updated_at", id, it.Name, it.Category, it.Specs[0], d.region, g.now.AddDate(0, 0, g.i(7, 20)), it.Unit, price, ref, ref*8,
			d.status, requested, members[0].Owner.ID, created, created)
		for k, o := range members {
			qty := niceQty(ref * g.f(1.5, 4))
			at := created.Add(hrs(float64(k) * g.f(5, 40)))
			g.ins("pool_members", "pool_id, org_id, quantity, opt_in, drop_point, created_at, updated_at", id, o.ID, qty, k%2 == 0, o.Def.City, at, at)
			g.procurement(o, it, qty, "in_collective", &id, at)
		}
	}

	states := []string{"draft", "pending_approval", "approved", "published", "rejected", "cancelled", "awarded", "pending_approval"}
	for k, o := range g.orgs {
		if len(o.Def.Buy) == 0 {
			continue
		}
		it := demoPick(g, demoItems)
		for _, cat := range o.Def.Buy {
			if cand := itemsIn(cat); len(cand) > 0 {
				it = demoPick(g, cand)
				break
			}
		}
		g.procurement(o, it, niceQty(g.f(it.QtyMin, it.QtyMax)), states[k%len(states)], nil, g.when())
	}
}

func itemsIn(cat string) []demoItem {
	var out []demoItem
	for _, it := range demoItems {
		if it.Category == cat {
			out = append(out, it)
		}
	}
	return out
}

var demoRules = []approvalRule{{"Procurement > Rp 50 jt", 50_000_000, []string{"finance", "owner"}, []string{"procurement", "auction"}},
	{"Auction > Rp 200 jt", 200_000_000, []string{"owner", "procurement"}, []string{"auction"}}}

func (g *demoGen) procurement(o *demoOrg, it demoItem, qty float64, status string, pool *string, created time.Time) string {
	budget := nicePrice(float64(it.PriceMax) * qty)
	required := requiredApprovers(budget, "procurement", demoRules)
	id := newID()
	by := o.member("procurement")
	g.ins("procurement_requests", "id, org_id, need, category_id, quantity, unit, budget_idr, deadline, spec, delivery_location, visibility, status, "+
		"required_approvers, pool_id, created_by, created_at, updated_at", id, o.ID, it.Name, it.Category, qty, it.Unit, budget, created.AddDate(0, 0, g.i(14, 45)),
		demoPick(g, it.Specs), o.Def.City, map[bool]string{true: "aggregate", false: "public"}[pool != nil], status, required, pool, by.ID, created, created)
	decided := map[string]int{"pending_approval": 1, "approved": 9, "published": 9, "in_auction": 9, "in_collective": 9, "awarded": 9, "po_issued": 9, "rejected": 1}[status]
	for k, role := range required {
		if k >= decided {
			break
		}
		m := o.member(role)
		decision, note := "approved", any(nil)
		if status == "rejected" {
			decision, note = "rejected", "Anggaran bulan ini sudah terpakai"
		}
		g.ins("procurement_approvals", "procurement_request_id, role, decision, note, decided_by, decided_at, on_behalf", id, role, decision, note, m.ID,
			created.Add(hrs(float64(k+1)*g.f(2, 20))), m.Role != role)
	}
	return id
}

func (g *demoGen) seedPurchaseHistory() {
	var ids []string
	var supplierOrgs []*demoOrg
	for _, o := range g.orgs {
		if o.SupplierID != "" {
			supplierOrgs = append(supplierOrgs, o)
		}
	}
	if len(supplierOrgs) == 0 {
		return
	}
	months := max(3, g.days/30) + 3
	for _, o := range g.orgs {
		if len(o.Def.Buy) == 0 || o.Def.Verification == "unverified" {
			continue
		}
		items := itemsIn(o.Def.Buy[0])
		n := 0
		for k := months; k >= 1; k-- {
			month := time.Date(g.now.Year(), g.now.Month()-time.Month(k), 1, 0, 0, 0, 0, time.UTC)
			for range g.i(1, 3) {
				it := demoPick(g, items)
				sup := demoPick(g, supplierOrgs)
				market := float64(it.PriceMin+it.PriceMax) / 2 * (1 + 0.01*float64(months-k))
				paid := nicePrice(market * g.f(0.9, 1.02))
				via := []string{"auction", "direct", "collective"}[g.r.IntN(3)]
				var bidders, opening any
				if via == "auction" {
					bidders, opening = g.i(3, 9), nicePrice(market*1.08)
				}
				n++
				id := newID()
				ids = append(ids, id)
				g.ins("org_purchase_history", "id, org_id, code, month, item, category_id, supplier_id, quantity, unit, unit_price_idr, budget_unit_idr, "+
					"market_unit_idr, via, bidders, opening_idr, created_at", id, o.ID, fmt.Sprintf("HIS-%s-%03d", strings.ToUpper(o.Slug[:3]), n), month, it.Name,
					it.Category, sup.SupplierID, niceQty(g.f(it.QtyMin, it.QtyMax)), it.Unit, paid, nicePrice(market*1.05), nicePrice(market), via, bidders, opening, month)
			}
		}
	}
	g.flush()
	g.q(`UPDATE outbox SET payload = payload || '{"demo": true}' WHERE topic IN ('trade.status','auction.closed') AND aggregate_id = ANY($1)`, ids)
}

func (g *demoGen) seedWithdrawals() {
	banks := []string{"BCA", "BNI", "BRI", "Mandiri", "BSI"}
	n := 0
	for _, p := range g.parties {
		if n >= 10 || g.err != nil {
			break
		}
		at := g.ago(g.f(2, 10))
		var bal int64
		for _, m := range g.avail[p.ID] {
			if !m.at.After(at) {
				bal += m.amount
			}
		}
		if bal < 5_000_000 {
			continue
		}
		n++
		amount := nicePrice(float64(bal) * g.f(0.4, 0.8))
		acctNo := fmt.Sprintf("%010d", g.r.Int64N(9_000_000_000)+1_000_000_000)
		sealed, err := secure.Encrypt(g.s.Keys.BankCipher, []byte(acctNo), []byte(p.ID))
		if err != nil {
			g.err = err
			return
		}
		bankID, bank := newID(), demoPick(g, banks)
		g.ins("bank_accounts", "id, party_id, bank, holder, account_no_enc, account_last4, created_by, created_at", bankID, p.ID, bank, p.Name, sealed,
			acctNo[len(acctNo)-4:], p.Actor.ID, at.Add(-time.Hour))
		wid, code := newID(), g.code("WDR")
		status := "paid"
		if n == 1 {
			status = "processing"
		}
		var paidAt, ref, decided, by any
		if status == "paid" {
			pa := at.Add(hrs(g.f(4, 40)))
			paidAt, ref, decided, by = pa, fmt.Sprintf("TRF%d", g.r.Int64N(90_000_000)+10_000_000), pa, g.staff.ID
		}
		g.ins("withdrawals", "id, code, party_id, bank_account_id, amount_idr, status, requested_by, paid_at, transfer_ref, decided_by, decided_at, created_at, updated_at",
			wid, code, p.ID, bankID, amount, status, p.Actor.ID, paidAt, ref, by, decided, at, at)
		g.journal("Tarik dana ke "+bank, at, nil, &wid, p.actorID(),
			demoLine{"wallet_available:" + p.ID, amount, "withdrawal"}, demoLine{"payout_pending:", -amount, "withdrawal"})
		if status == "paid" {
			g.journal("Transfer "+code+" ke "+bank+" ••"+acctNo[len(acctNo)-4:]+" · ref "+ref.(string), paidAt.(time.Time), nil, &wid, &g.staff.ID,
				demoLine{"payout_pending:", amount, "withdrawal"}, demoLine{"bank_clearing:", -amount, "withdrawal"})
		}
	}
}

func (g *demoGen) detectOpportunities() error {
	ctx, tx := g.ctx, g.tx
	if err := detectPass(ctx, tx); err != nil {
		return err
	}
	demo := make([]string, len(g.listings))
	for i, l := range g.listings {
		demo[i] = l.ID
	}
	rows, err := tx.Query(ctx, `
		SELECT o.id::text FROM opportunities o WHERE o.created_at = now()
		  AND NOT EXISTS (SELECT 1 FROM opportunity_listings ol WHERE ol.opportunity_id = o.id AND NOT ol.listing_id = ANY($1::uuid[]))
		ORDER BY o.potential_value_idr DESC, o.id`, demo)
	if err != nil {
		return err
	}
	ids, err := collectStrings(rows)
	if err != nil {
		return err
	}
	stmts := []string{
		`UPDATE opportunities o SET detected_at = x.t, created_at = x.t FROM (
			SELECT ol.opportunity_id, (array_agg(l.created_at ORDER BY l.created_at))[least(2, count(*))] + interval '37 minutes' AS t
			FROM opportunity_listings ol JOIN listings l ON l.id = ol.listing_id GROUP BY 1) x
		 WHERE o.id = x.opportunity_id AND o.id = ANY($1::uuid[])`,
		`UPDATE opportunity_listings ol SET created_at = o.detected_at FROM opportunities o WHERE o.id = ol.opportunity_id AND o.id = ANY($1::uuid[])`,
		`UPDATE outbox b SET created_at = o.detected_at, payload = b.payload || '{"demo": true}' FROM opportunities o
		 WHERE b.created_at = now() AND b.topic = 'opportunity.detected' AND b.aggregate_id = o.id::text AND o.id = ANY($1::uuid[])`,
		`UPDATE outbox b SET created_at = o.detected_at, payload = b.payload || '{"demo": true}' FROM opportunities o
		 WHERE b.created_at = now() AND b.topic = 'activity' AND b.payload->>'title' = 'Opportunity baru: ' || o.title AND o.id = ANY($1::uuid[])`,
		`DELETE FROM outbox b USING opportunities o WHERE b.created_at = now() AND b.topic = 'rt' AND o.id = ANY($1::uuid[])
		   AND (b.payload::text LIKE '%' || o.id::text || '%' OR b.payload->'payload'->>'title' = 'Opportunity baru: ' || o.title)`,
		`DELETE FROM notifications WHERE created_at = now() AND substring(href FROM '[0-9a-f]{8}-[0-9a-f-]{27}')::uuid = ANY($1::uuid[])`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s, ids); err != nil {
			return err
		}
	}
	g.n["opportunities"] = len(ids)
	if len(g.mmOrgs) == 0 {
		return nil
	}
	for k, id := range ids {
		stage := []string{"evaluating", "forming", "", "dismissed", "evaluating", "", ""}[k%7]
		if stage == "" {
			continue
		}
		var reason any
		if stage == "dismissed" {
			reason = "Volume terlalu kecil untuk satu market; pantau 1 bulan lagi."
		}
		by := g.mmOrgs[k%len(g.mmOrgs)].Owner.ID
		if _, err := tx.Exec(ctx, `INSERT INTO mm_pipeline (opportunity_id, stage, dismiss_reason, updated_by) VALUES ($1, $2, $3, $4)`, id, stage, reason, by); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE opportunities SET status = CASE WHEN $2 = 'evaluating' THEN 'detected' ELSE $2 END WHERE id = $1`, id, stage); err != nil {
			return err
		}
		g.n["mm_pipeline"]++
	}

	users := g.userParties()
	for k, id := range ids {
		if k >= 8 {
			break
		}
		for j := range 3 {
			p := users[(k*5+j*11)%len(users)]
			if _, err := tx.Exec(ctx, `INSERT INTO opportunity_follows (user_id, opportunity_id, created_at) VALUES ($1, $2, now() - interval '2 days') ON CONFLICT DO NOTHING`,
				*p.UserID, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func collectStrings(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

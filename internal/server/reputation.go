package server

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Reputation (PRD §8.10): port of the frontend's src/domain/reputation.ts (same weights, so scores match across
// clients), computed from a party's trades with their status timeline, disputes and the reviews the other side wrote.

// repTx is one trade as seen from the party being scored.
type repTx struct {
	ID, Title, Status string
	Counterparty      string // counterparty party id (the mock keys repeat partners by name)
	TotalIdr          int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DueAt             time.Time
	Timeline          []repStep
	Dispute           string   // status of the latest dispute, "" when none
	Rating            *float64 // the review the other side wrote, if any
}

type repStep struct {
	Status string
	At     time.Time
}

// baselineScore is the score of an account with no finished transactions yet.
const baselineScore = 80

var finishedStatus = map[string]bool{"completed": true, "cancelled": true, "disputed": true}

// jsRound is Math.round (half up), so scores match the frontend at .5.
func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

func ratio(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	v := float64(n) / float64(d)
	return &v
}

func (t repTx) completedAt() time.Time {
	for _, s := range t.Timeline {
		if s.Status == "completed" {
			return s.At
		}
	}
	return t.UpdatedAt
}

func reputationScore(txs []repTx) (int, api.ReputationBreakdown, api.ReputationCounts) {
	var completed, cancelled, disputes, finished int
	seen := map[string]int{}
	for _, t := range txs {
		seen[t.Counterparty]++
	}
	var onTime, repeat int
	var volume int64
	var ratings, responses []float64
	for _, t := range txs {
		switch t.Status {
		case "completed":
			completed++
			volume += t.TotalIdr
			if !t.completedAt().After(t.DueAt) {
				onTime++
			}
			if seen[t.Counterparty] > 1 {
				repeat++
			}
		case "cancelled":
			cancelled++
		}
		if finishedStatus[t.Status] {
			finished++
		}
		if t.Dispute != "" || t.Status == "disputed" {
			disputes++
		}
		if t.Rating != nil {
			ratings = append(ratings, *t.Rating)
		}
		if len(t.Timeline) > 1 {
			if h := t.Timeline[1].At.Sub(t.CreatedAt).Hours(); h >= 0 {
				responses = append(responses, h)
			}
		}
	}
	avg1 := func(xs []float64) *float64 {
		if len(xs) == 0 {
			return nil
		}
		var s float64
		for _, x := range xs {
			s += x
		}
		v := jsRound(s/float64(len(xs))*10) / 10
		return &v
	}
	b := api.ReputationBreakdown{
		FulfillmentRate:  ratio(completed, finished),
		OnTimeRate:       ratio(onTime, completed),
		CancellationRate: ratio(cancelled, len(txs)),
		DisputeRate:      ratio(disputes, len(txs)),
		VolumeIdr:        int(volume),
		RepeatRate:       ratio(repeat, completed),
		ResponseHours:    avg1(responses),
		RatingAvg:        avg1(ratings),
		RatingCount:      len(ratings),
	}
	or0 := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	raw := float64(baselineScore)
	if finished > 0 {
		raw = 40*or0(b.FulfillmentRate) + 20*or0(b.OnTimeRate) + 15*(1-or0(b.CancellationRate)) + 15*(1-or0(b.DisputeRate)) +
			5*or0(b.RepeatRate) + 5*math.Min(1, float64(completed)/20)
	}
	// Reviews weigh 10%: 1 star -> 0, 5 stars -> 10 points.
	if b.RatingAvg != nil {
		raw = 0.9*raw + 10*((*b.RatingAvg-1)/4)
	}
	score := int(jsRound(math.Min(100, math.Max(0, raw))))
	return score, b, api.ReputationCounts{Transactions: len(txs), Successful: completed, Disputes: disputes, Cancelled: cancelled}
}

var reputationEventTitle = map[string]string{"completed": "Transaksi selesai", "cancelled": "Transaksi dibatalkan", "disputed": "Dispute dibuka"}

// reputationReport is the full report; `now` decides which 12 months the trend covers.
func reputationReport(txs []repTx, now time.Time) api.ReputationReport {
	byTime := slices.Clone(txs)
	slices.SortStableFunc(byTime, func(a, b repTx) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	events := []api.ReputationEvent{}
	prev := baselineScore
	for i, t := range byTime {
		if !finishedStatus[t.Status] {
			continue
		}
		score, _, _ := reputationScore(byTime[:i+1])
		suffix := ""
		if t.Dispute == "resolved" {
			suffix = " (dispute diselesaikan)"
		} else if t.Dispute != "" {
			suffix = " (dengan dispute)"
		}
		events = append(events, api.ReputationEvent{Id: t.ID, At: t.UpdatedAt, Title: reputationEventTitle[t.Status] + suffix + ": " + t.Title,
			Delta: float64(score - prev), Score: float64(score)})
		prev = score
	}
	slices.Reverse(events)

	r := api.ReputationReport{Events: events}
	r.Score, r.Breakdown, r.Counts = reputationScore(txs)
	now = now.UTC()
	for k := range 12 {
		month := time.Date(now.Year(), now.Month()-time.Month(11-k), 1, 0, 0, 0, 0, time.UTC)
		end := month.AddDate(0, 1, 0)
		var upTo []repTx
		for _, t := range byTime {
			if t.UpdatedAt.Before(end) {
				upTo = append(upTo, t)
			}
		}
		score, _, _ := reputationScore(upTo)
		r.Trend = append(r.Trend, struct {
			Month string `json:"month"`
			Score int    `json:"score"`
		}{month.Format("2006-01"), score})
	}
	return r
}

// loadRepTxs reads every trade where one of the parties is buyer or supplier, from that side's point of view.
func loadRepTxs(ctx context.Context, q dbtx, partyIDs []string) ([]repTx, error) {
	if len(partyIDs) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT t.id, t.title, t.status, t.total_idr, t.created_at, t.updated_at, t.due_at,
		       CASE WHEN t.buyer_party_id = ANY($1::uuid[]) THEN t.supplier_party_id ELSE t.buyer_party_id END,
		       coalesce((SELECT d.status FROM disputes d WHERE d.trade_id = t.id ORDER BY d.opened_at DESC LIMIT 1), ''),
		       (SELECT r.rating::float8 FROM reviews r WHERE r.trade_id = t.id
		          AND r.side = CASE WHEN t.buyer_party_id = ANY($1::uuid[]) THEN 'supplier' ELSE 'buyer' END),
		       coalesce((SELECT array_agg(e.status ORDER BY e.at, e.id) FROM trade_events e WHERE e.trade_id = t.id), '{}'),
		       coalesce((SELECT array_agg(e.at ORDER BY e.at, e.id) FROM trade_events e WHERE e.trade_id = t.id), '{}')
		FROM trades t
		WHERE t.buyer_party_id = ANY($1::uuid[]) OR t.supplier_party_id = ANY($1::uuid[])`, partyIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []repTx
	for rows.Next() {
		var t repTx
		var statuses []string
		var ats []time.Time
		if err := rows.Scan(&t.ID, &t.Title, &t.Status, &t.TotalIdr, &t.CreatedAt, &t.UpdatedAt, &t.DueAt, &t.Counterparty,
			&t.Dispute, &t.Rating, &statuses, &ats); err != nil {
			return nil, err
		}
		for i := range statuses {
			t.Timeline = append(t.Timeline, repStep{statuses[i], ats[i]})
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// partyReputation is the public summary (score + counts) of a set of parties (a user's party, or an org's).
func partyReputation(ctx context.Context, q dbtx, partyIDs []string) (api.ProfileReputation, error) {
	txs, err := loadRepTxs(ctx, q, partyIDs)
	if err != nil {
		return api.ProfileReputation{}, err
	}
	score, _, counts := reputationScore(txs)
	return api.ProfileReputation{Score: score, Counts: counts}, nil
}

// userPartyIDs is the user's party (no row yet = no trades).
func userPartyIDs(ctx context.Context, q dbtx, userID string) ([]string, error) {
	var ids []string
	rows, err := q.Query(ctx, `SELECT id::text FROM parties WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Server) GetMyReputation(ctx context.Context, _ api.GetMyReputationRequestObject) (api.GetMyReputationResponseObject, error) {
	sess, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Reader()
	parties, err := userPartyIDs(ctx, q, sess.UserID)
	if err != nil {
		return nil, err
	}
	txs, err := loadRepTxs(ctx, q, parties)
	if err != nil {
		return nil, err
	}
	return api.GetMyReputation200JSONResponse(reputationReport(txs, time.Now())), nil
}

// reputationOf is the score of a set of parties and how many trades it rests on (0 = new account, baseline score).
func reputationOf(ctx context.Context, q dbtx, partyIDs ...string) (score, trades int, err error) {
	r, err := partyReputation(ctx, q, partyIDs)
	return r.Score, r.Counts.Transactions, err
}

// userReputation is reputationOf the user's own party.
func userReputation(ctx context.Context, q dbtx, userID string) (score, trades int, err error) {
	ids, err := userPartyIDs(ctx, q, userID)
	if err != nil {
		return 0, 0, err
	}
	return reputationOf(ctx, q, ids...)
}

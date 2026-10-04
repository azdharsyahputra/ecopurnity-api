package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

var botSuppliers = map[string][]string{
	"agri":          {"Koperasi Mitra Tani", "CV Tani Lestari", "PT Agro Priangan"},
	"food":          {"CV Sumber Pangan", "PT Rasa Nusantara", "UD Makmur Jaya"},
	"packaging":     {"PT Kemas Prima", "CV Plastik Jaya", "UD Sinar Pack"},
	"manufacturing": {"PT Polimer Jaya", "CV Logam Mandiri", "PT Karya Teknik"},
	"logistics":     {"PT Logistik Andalan", "CV Angkut Cepat", "PT Dingin Nusantara"},
	"it":            {"Jogja Digital Hub", "CV Kode Kreatif", "PT Solusi Teknologi"},
	"energy":        {"PT Surya Bali", "CV Energi Hijau", "PT Daya Mandiri"},
}

var (
	botTerms   = []string{"escrow", "net14", "net30"}
	botNotes   = []string{"Stok siap, bisa kirim minggu ini.", "Termasuk ongkos kirim ke lokasi.", "Bisa bertahap sesuai jadwal."}
	botReplies = []string{
		"Baik, kami cek ketersediaan stok dan jadwal kirimnya dulu.",
		"Bisa. Untuk kuantitas segitu kami siap kirim bertahap.",
		"Terima kasih, spesifikasinya sudah kami catat. Harga sudah termasuk ongkos muat.",
	}
)

func (s *Server) RunCounterparties(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.CounterpartyTick(ctx); err != nil && ctx.Err() == nil && s.Log != nil {
			s.Log.Error("simulated counterparties", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) CounterpartyTick(ctx context.Context) error {
	for _, step := range []struct {
		due string
		run func(context.Context, pgx.Tx, string) error
	}{
		{`SELECT id::text FROM rfqs WHERE status = 'open' AND created_at > now() - interval '10 minutes' AND created_at <= now() - interval '6 seconds'`, s.botQuotes},
		{`SELECT q.id::text FROM quotes q JOIN parties p ON p.id = q.supplier_party_id
		  WHERE q.status = 'countered' AND p.kind = 'external' AND (SELECT max(e.at) FROM quote_events e WHERE e.quote_id = q.id)
		        BETWEEN now() - interval '10 minutes' AND now() - interval '6 seconds'`, s.botCounter},
		{`SELECT c.id::text FROM conversations c
		  WHERE c.last_message_at > now() - interval '10 minutes' AND c.last_message_at <= now() - interval '5 seconds'
		    AND EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = c.id AND m.seq = c.message_seq AND m.author_user_id IS NOT NULL)
		    AND EXISTS (SELECT 1 FROM conversation_participants cp JOIN parties p ON p.id = cp.party_id
		                WHERE cp.conversation_id = c.id AND p.kind = 'external')`, s.botChat},
	} {
		rows, _ := s.DB.Primary().Query(ctx, step.due)
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := s.inTx(ctx, func(tx pgx.Tx) error { return step.run(ctx, tx, id) }); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) botQuotes(ctx context.Context, tx pgx.Tx, id string) error {
	var r rfqRow
	var age time.Duration
	err := tx.QueryRow(ctx, `
		SELECT r.id::text, r.code, r.buyer_party_id::text, p.user_id::text, r.item, r.category_id, r.unit, r.quantity::float8,
		       r.conversation_id::text, r.target_price_idr, now() - r.created_at
		FROM rfqs r JOIN parties p ON p.id = r.buyer_party_id WHERE r.id = $1 AND r.status = 'open' FOR UPDATE OF r SKIP LOCKED`, id).
		Scan(&r.ID, &r.Code, &r.BuyerParty, &r.BuyerUser, &r.Item, &r.Category, &r.Unit, &r.Quantity, &r.ConversationID, &r.TargetPrice, &age)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	type bot struct {
		party, name string
	}
	rows, _ := tx.Query(ctx, `
		SELECT p.id::text, p.name FROM rfq_invitations i JOIN parties p ON p.id = i.party_id
		WHERE i.rfq_id = $1 AND p.kind = 'external' ORDER BY i.invited_at, p.name`, r.ID)
	bots, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (bot, error) {
		var b bot
		return b, row.Scan(&b.party, &b.name)
	})
	if err != nil {
		return err
	}
	for _, name := range botSuppliers[r.Category] {
		bots = append(bots, bot{name: name})
	}
	ref := int64(10_000)
	if r.TargetPrice != nil {
		ref = *r.TargetPrice
	} else if err := tx.QueryRow(ctx, `SELECT round((price_min_idr + price_max_idr) / 2.0)::bigint FROM markets WHERE category_id = $1 ORDER BY created_at, id LIMIT 1`,
		r.Category).Scan(&ref); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	for i, b := range bots[:min(3, len(bots))] {
		if age < 6*time.Second+time.Duration(i)*5*time.Second {
			break
		}
		var quoted bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM quotes q JOIN parties p ON p.id = q.supplier_party_id WHERE q.rfq_id = $1 AND p.name = $2)`,
			r.ID, b.name).Scan(&quoted); err != nil {
			return err
		}
		if quoted {
			continue
		}
		if b.party == "" {
			if b.party, err = externalParty(ctx, tx, b.name, "business", i != 2); err != nil {
				return err
			}
		}
		price := int64(math.Round(float64(ref) * (0.94 + float64(i)*0.05)))
		var qid string
		if err := tx.QueryRow(ctx, `
			INSERT INTO quotes (rfq_id, supplier_party_id, price_idr, quantity, lead_time_days, terms, note, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, clock_timestamp()) RETURNING id::text`,
			r.ID, b.party, price, r.Quantity, 3+i*2, botTerms[i%3], botNotes[i%3]).Scan(&qid); err != nil {
			return err
		}
		if err := quoteEvent(ctx, tx, qid, nil, b.name, "Penawaran dikirim"); err != nil {
			return err
		}
		if err := addParticipant(ctx, tx, r.ConversationID, convParty{PartyID: b.party}); err != nil {
			return err
		}
		if r.BuyerUser != nil {
			if err := notify(ctx, tx, *r.BuyerUser, notification{Type: "transaction_update", Title: "Penawaran baru untuk " + r.Code,
				Body: b.name + ": " + unitPrice(price, r.Unit), Href: "/app/rfq/" + r.ID}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) botCounter(ctx context.Context, tx pgx.Tx, quoteID string) error {
	var rfqID string
	if err := tx.QueryRow(ctx, `SELECT rfq_id::text FROM quotes WHERE id = $1`, quoteID).Scan(&rfqID); err != nil {
		return err
	}

	if err := tx.QueryRow(ctx, `SELECT id::text FROM rfqs WHERE id = $1 FOR UPDATE SKIP LOCKED`, rfqID).Scan(&rfqID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	r, err := lockRfq(ctx, tx, rfqID)
	if err != nil {
		return err
	}
	qt, err := lockQuote(ctx, tx, rfqID, quoteID)
	if err != nil {
		return err
	}
	if r.Status != "open" || qt.Status != "countered" || qt.Counter == nil {
		return nil
	}
	accept, revise := botCounterReply(qt.Price, *qt.Counter)
	if accept {

		_, err := acceptQuote(ctx, tx, r, qt, *qt.Counter, nil, qt.SupplierName)
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE quotes SET status = 'submitted', price_idr = $2, counter_price_idr = NULL WHERE id = $1`, qt.ID, revise); err != nil {
		return err
	}
	if err := quoteEvent(ctx, tx, qt.ID, nil, qt.SupplierName, "Revisi "+rupiah(revise)); err != nil {
		return err
	}
	if r.BuyerUser == nil {
		return nil
	}
	return notify(ctx, tx, *r.BuyerUser, notification{Type: "transaction_update", Title: qt.SupplierName + " merevisi harga",
		Body: fmt.Sprintf("%s · %s", r.Code, unitPrice(revise, r.Unit)), Href: "/app/rfq/" + r.ID})
}

func (s *Server) botChat(ctx context.Context, tx pgx.Tx, conv string) error {
	var seq int64
	var human bool
	err := tx.QueryRow(ctx, `
		SELECT c.message_seq, EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = c.id AND m.seq = c.message_seq AND m.author_user_id IS NOT NULL)
		FROM conversations c WHERE c.id = $1 FOR UPDATE SKIP LOCKED`, conv).Scan(&seq, &human)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !human) {
		return nil
	}
	if err != nil {
		return err
	}
	var party, name string
	if err := tx.QueryRow(ctx, `
		SELECT p.id::text, p.name FROM conversation_participants cp JOIN parties p ON p.id = cp.party_id
		WHERE cp.conversation_id = $1 AND p.kind = 'external' ORDER BY cp.joined_at, cp.id LIMIT 1`, conv).Scan(&party, &name); err != nil {
		return err
	}
	_, err = postMessage(ctx, tx, conv, convParty{PartyID: party}, name, botReplies[seq%int64(len(botReplies))], nil)
	return err
}

package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

// Procurement requests (PRD 9.4) and collective pools (PRD 9.5).

var errProcurementNotFound = notFound("Procurement tidak ditemukan")

func loadProcurements(ctx context.Context, q dbtx, where string, args ...any) ([]api.ProcurementRequest, error) {
	rows, err := q.Query(ctx, `
		SELECT r.id::text, r.code, r.need, r.category_id, r.quantity, r.unit, r.budget_idr, r.deadline, r.spec, r.delivery_location, r.visibility,
		       r.status, r.required_approvers, r.pool_id::text, r.created_at, r.updated_at,
		       (SELECT a.id::text FROM org_auctions a WHERE a.procurement_request_id = r.id AND a.status <> 'rejected'),
		       array(SELECT i.supplier_id::text FROM org_procurement_invites i WHERE i.procurement_request_id = r.id ORDER BY 1),
		       `+memberLabelSQL("u.name", "u.id", "r.org_id")+`
		FROM procurement_requests r JOIN users u ON u.id = r.created_by WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (api.ProcurementRequest, error) {
		var r api.ProcurementRequest
		return r, row.Scan(&r.Id, &r.Code, &r.Need, &r.CategoryId, &r.Quantity.Value, &r.Quantity.Unit, &r.BudgetIdr, &r.Deadline, &r.Spec,
			&r.DeliveryLocation, &r.Visibility, &r.Status, &r.RequiredApprovers, &r.PoolId, &r.CreatedAt, &r.UpdatedAt, &r.AuctionId,
			&r.InvitedSupplierIds, &r.CreatedBy)
	})
	if err != nil || len(out) == 0 {
		return nonNil(out), err
	}
	ids := make([]string, len(out))
	idx := map[string]int{}
	for i, r := range out {
		ids[i], idx[r.Id] = r.Id, i
		out[i].Approvals = []api.Approval{}
	}
	rows, err = q.Query(ctx, `
		SELECT a.procurement_request_id::text, a.role, a.decision, a.note, a.decided_at,
		       u.name || ' (' || coalesce((SELECT ro.label FROM org_roles ro WHERE ro.org_id = r.org_id AND ro.key = a.role), a.role) || ')'
		FROM procurement_approvals a JOIN procurement_requests r ON r.id = a.procurement_request_id JOIN users u ON u.id = a.decided_by
		WHERE a.procurement_request_id::text = ANY($1) ORDER BY a.decided_at, a.id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var a api.Approval
		if err := rows.Scan(&id, &a.Role, &a.Decision, &a.Note, &a.At, &a.By); err != nil {
			return nil, err
		}
		out[idx[id]].Approvals = append(out[idx[id]].Approvals, a)
	}
	return out, rows.Err()
}

func loadProcurement(ctx context.Context, q dbtx, orgID, id string) (api.ProcurementRequest, error) {
	rs, err := loadProcurements(ctx, q, `r.org_id = $1 AND r.id::text = $2`, orgID, id)
	if err == nil && len(rs) == 0 {
		err = errProcurementNotFound
	}
	if err != nil {
		return api.ProcurementRequest{}, err
	}
	return rs[0], nil
}

func (s *Server) ListOrgProcurement(ctx context.Context, req api.ListOrgProcurementRequestObject) (api.ListOrgProcurementResponseObject, error) {
	q := s.DB.Primary()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	out, err := loadProcurements(ctx, q, `r.org_id = $1 ORDER BY r.updated_at DESC, r.id`, c.OrgID)
	if err != nil {
		return nil, err
	}
	return api.ListOrgProcurement200JSONResponse(out), nil
}

func (s *Server) GetOrgProcurement(ctx context.Context, req api.GetOrgProcurementRequestObject) (api.GetOrgProcurementResponseObject, error) {
	q := s.DB.Primary()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	r, err := loadProcurement(ctx, q, c.OrgID, req.Id)
	if err != nil {
		return nil, err
	}
	out := api.GetOrgProcurement200JSONResponse{Request: r}
	if out.Activity, err = loadAuditEntries(ctx, q, `org_id = $1 AND entity_id IN ($2, $3) ORDER BY at DESC, id DESC`, c.OrgID, r.Id, deref(r.AuctionId)); err != nil {
		return nil, err
	}
	if r.PoolId != nil {
		pools, err := loadPools(ctx, q, c.OrgID, `p.id = $1`, *r.PoolId)
		if err != nil {
			return nil, err
		}
		if len(pools) > 0 {
			p := poolOnly(pools[0])
			out.Pool = &p
		}
	}
	return out, nil
}

func (s *Server) CreateOrgProcurement(ctx context.Context, req api.CreateOrgProcurementRequestObject) (api.CreateOrgProcurementResponseObject, error) {
	in := req.Body
	var out api.ProcurementRequest
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("procurement", "create"); err != nil {
			return err
		}
		f := map[string]string{}
		if strings.TrimSpace(in.Need) == "" {
			f["need"] = "Kebutuhan wajib diisi"
		}
		if !(in.Quantity.Value > 0) || strings.TrimSpace(in.Quantity.Unit) == "" {
			f["quantity"] = "Kuantitas harus lebih dari 0"
		}
		if in.BudgetIdr <= 0 {
			f["budgetIdr"] = "Budget harus lebih dari 0"
		}
		if !in.Deadline.After(time.Now()) {
			f["deadline"] = "Deadline harus di masa depan"
		}
		invited := []string{}
		for _, id := range in.InvitedSupplierIds {
			if !slices.Contains(invited, id) {
				invited = append(invited, id)
			}
		}
		if in.Visibility == "invite" && len(invited) == 0 {
			f["invited"] = "Pilih minimal satu supplier untuk diundang"
		}
		if in.Visibility != "invite" {
			invited = []string{}
		}
		if len(invited) > 0 {
			var known int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM suppliers WHERE id::text = ANY($1)`, invited).Scan(&known); err != nil {
				return err
			}
			if known != len(invited) {
				f["invited"] = "Supplier tidak ditemukan"
			}
		}
		if err := invalid("Periksa kembali isian procurement", f); err != nil {
			return err
		}
		rules, err := loadApprovalRules(ctx, tx, c.OrgID)
		if err != nil {
			return err
		}
		required := requiredApprovers(int64(in.BudgetIdr), "procurement", rules)
		status := "draft"
		if in.Submit {
			status = statusAfterApproval(required, nil)
		}
		var id, code string
		if err := tx.QueryRow(ctx, `
			INSERT INTO procurement_requests (org_id, need, category_id, quantity, unit, budget_idr, deadline, spec, delivery_location, visibility, status,
			                                  required_approvers, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id::text, code`,
			c.OrgID, strings.TrimSpace(in.Need), in.CategoryId, in.Quantity.Value, strings.TrimSpace(in.Quantity.Unit), in.BudgetIdr, in.Deadline,
			strings.TrimSpace(in.Spec), strings.TrimSpace(in.DeliveryLocation), in.Visibility, status, required, c.sess.UserID).Scan(&id, &code); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO org_procurement_invites (procurement_request_id, supplier_id) SELECT $1, unnest($2::uuid[])`, id, invited); err != nil {
			return err
		}
		action := "Simpan draft procurement"
		if in.Submit {
			action = "Ajukan procurement"
		}
		need := strings.TrimSpace(in.Need)
		if err := c.audit(ctx, tx, action, "procurement", id, code+" "+need, nil); err != nil {
			return err
		}
		if status == "pending_approval" {
			if err := askApprovers(ctx, tx, c, "procurement", id, code, need, int64(in.BudgetIdr), required, nil); err != nil {
				return err
			}
		}
		out, err = loadProcurement(ctx, tx, c.OrgID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CreateOrgProcurement201JSONResponse(out), nil
}

var procurementAuditLabel = map[string]string{"submit": "Ajukan procurement", "publish": "Publish procurement", "cancel": "Batalkan procurement",
	"collective": "Gabungkan ke collective"}

func (s *Server) ActOnOrgProcurement(ctx context.Context, req api.ActOnOrgProcurementRequestObject) (api.ActOnOrgProcurementResponseObject, error) {
	in := req.Body
	action := string(in.Action)
	var out api.ProcurementRequest
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		var id string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM procurement_requests WHERE org_id = $1 AND id::text = $2 FOR UPDATE`, c.OrgID, req.Id).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
			return errProcurementNotFound
		} else if err != nil {
			return err
		}
		r, err := loadProcurement(ctx, tx, c.OrgID, id)
		if err != nil {
			return err
		}
		approvals := apiApprovals(r.Approvals)
		if !slices.Contains(procurementActions(string(r.Status), r.RequiredApprovers, approvals, c.Role, c.Perms), action) {
			return conflict("invalid_action", "Aksi ini tidak tersedia untuk status atau peranmu sekarang")
		}
		before, status := string(r.Status), string(r.Status)
		label := r.Code + " " + r.Need
		switch action {
		case "approve", "reject":
			decision := map[string]string{"approve": "approved", "reject": "rejected"}[action]
			note := strings.TrimSpace(deref(in.Note))
			if decision == "rejected" && note == "" {
				return invalid("Tulis alasan penolakan", map[string]string{"note": "Alasan wajib diisi saat menolak"})
			}
			if _, err := tx.Exec(ctx, `INSERT INTO procurement_approvals (procurement_request_id, role, decision, note, decided_by) VALUES ($1, $2, $3, nullif($4, ''), $5)`,
				id, c.Role, decision, note, c.sess.UserID); err != nil {
				return err
			}
			status = statusAfterApproval(r.RequiredApprovers, append(approvals, approval{c.Role, decision}))
			if _, err := tx.Exec(ctx, `UPDATE procurement_requests SET status = $2, updated_at = now() WHERE id = $1`, id, status); err != nil {
				return err
			}
			var reason *string
			auditAction := "Approve procurement"
			if decision == "rejected" {
				reason, auditAction = &note, "Tolak procurement"
			}
			return finishProcurement(ctx, tx, c, id, &out, func() error { return c.audit(ctx, tx, auditAction, "procurement", id, r.Code, reason) })
		case "submit":
			rules, err := loadApprovalRules(ctx, tx, c.OrgID)
			if err != nil {
				return err
			}
			required := requiredApprovers(int64(r.BudgetIdr), "procurement", rules)
			status = statusAfterApproval(required, nil)
			if _, err := tx.Exec(ctx, `UPDATE procurement_requests SET status = $2, required_approvers = $3 WHERE id = $1`, id, status, required); err != nil {
				return err
			}
			if status == "pending_approval" {
				if err := askApprovers(ctx, tx, c, "procurement", id, r.Code, r.Need, int64(r.BudgetIdr), required, nil); err != nil {
					return err
				}
			}
		case "publish", "cancel":
			status = map[string]string{"publish": "published", "cancel": "cancelled"}[action]
			if _, err := tx.Exec(ctx, `UPDATE procurement_requests SET status = $2 WHERE id = $1`, id, status); err != nil {
				return err
			}
		case "collective":
			qty := r.Quantity.Value
			if in.Quantity != nil {
				qty = *in.Quantity
			}
			if !(qty > 0) {
				return invalid("Isi kuantitas", map[string]string{"quantity": "Kuantitas harus > 0"})
			}
			var poolID string
			err := tx.QueryRow(ctx, `
				SELECT id::text FROM collective_pools WHERE category_id = $1 AND unit = $2 AND status = 'open' ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
				r.CategoryId, r.Quantity.Unit).Scan(&poolID)
			if errors.Is(err, pgx.ErrNoRows) {
				region, err := orgRegion(ctx, tx, c.OrgID)
				if err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, `
					INSERT INTO collective_pools (title, category_id, spec, region, deadline, unit, base_unit_price_idr, ref_qty, threshold_qty, created_by)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $8::numeric * 8, $9) RETURNING id::text`,
					r.Need, r.CategoryId, r.Spec, region, r.Deadline, r.Quantity.Unit, max(1, int64(math.Round(float64(r.BudgetIdr)/r.Quantity.Value))),
					r.Quantity.Value, c.sess.UserID).Scan(&poolID); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if _, err := joinPool(ctx, tx, c.OrgID, poolID, qty, in.OptIn != nil && *in.OptIn, r.DeliveryLocation); err != nil {
				return err
			}
			status = "in_collective"
			if _, err := tx.Exec(ctx, `UPDATE procurement_requests SET status = $2, pool_id = $3, visibility = 'aggregate' WHERE id = $1`, id, status, poolID); err != nil {
				return err
			}
		}
		return finishProcurement(ctx, tx, c, id, &out, func() error {
			return c.audit(ctx, tx, procurementAuditLabel[action], "procurement", id, label, nil, change{Field: "Status", Before: &before, After: status})
		})
	})
	if err != nil {
		return nil, err
	}
	return api.ActOnOrgProcurement200JSONResponse(out), nil
}

func finishProcurement(ctx context.Context, tx pgx.Tx, c *orgCtx, id string, out *api.ProcurementRequest, audit func() error) error {
	if err := audit(); err != nil {
		return err
	}
	r, err := loadProcurement(ctx, tx, c.OrgID, id)
	*out = r
	return err
}

// orgRegion: the org's public region, else the last part of its location ("Bandung, Jawa Barat"), else Jawa Barat.
func orgRegion(ctx context.Context, q dbtx, orgID string) (string, error) {
	var region string
	err := q.QueryRow(ctx, `SELECT coalesce(nullif(region, ''), location) FROM org_profiles WHERE org_id = $1`, orgID).Scan(&region)
	parts := strings.Split(region, ", ")
	if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
		return last, err
	}
	return "Jawa Barat", err
}

// ── Collective pools ─────────────────────────────────────────────

var errPoolNotFound = notFound("Pool tidak ditemukan")

// joinPool adds the org's demand to the pool or updates it; returns the previous quantity (0 when new). The member's
// drop point is the request's delivery location, else the org's first warehouse, else its profile location.
func joinPool(ctx context.Context, tx pgx.Tx, orgID, poolID string, qty float64, optIn bool, dropPoint string) (float64, error) {
	var prev float64
	err := tx.QueryRow(ctx, `SELECT coalesce((SELECT quantity FROM pool_members WHERE pool_id = $1 AND org_id = $2), 0)`, poolID, orgID).Scan(&prev)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO pool_members (pool_id, org_id, quantity, opt_in, drop_point)
		VALUES ($1, $2, $3, $4, coalesce(nullif($5, ''), (SELECT name FROM org_warehouses WHERE org_id = $2 ORDER BY created_at LIMIT 1),
		                                 (SELECT nullif(location, '') FROM org_profiles WHERE org_id = $2)))
		ON CONFLICT (pool_id, org_id) DO UPDATE SET quantity = EXCLUDED.quantity, opt_in = EXCLUDED.opt_in,
		  drop_point = coalesce(nullif($5, ''), pool_members.drop_point)`, poolID, orgID, qty, optIn, strings.TrimSpace(dropPoint))
	return prev, err
}

// loadPools: pools as orgID sees them. Other businesses are "Bisnis lain #n" (join order) unless they opted in; the
// org's own row is `mine`. Settlement lines are masked the same way and only the own line carries its transaction.
func loadPools(ctx context.Context, q dbtx, orgID, where string, args ...any) ([]api.PoolView, error) {
	rows, err := q.Query(ctx, `
		SELECT p.id::text, p.title, p.category_id, p.spec, p.region, p.deadline, p.unit, p.base_unit_price_idr, p.ref_qty, p.threshold_qty, p.status,
		       p.market_requested_at, p.market_id::text, p.auction_id::text, a.status, a.ends_at, s.settled_at, su.name, wp.name, s.price_idr,
		       coalesce(op.categories::text[] @> ARRAY[p.category_id::text], false)
		FROM collective_pools p LEFT JOIN auctions a ON a.id = p.auction_id LEFT JOIN settlements s ON s.id = p.settlement_id
		LEFT JOIN users su ON su.id = s.settled_by LEFT JOIN parties wp ON wp.id = s.winner_party_id
		LEFT JOIN org_profiles op ON op.org_id::text = `+fmt.Sprintf("$%d", len(args)+1)+`
		WHERE `+where, append(args, orgID)...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.PoolView, error) {
		var p api.PoolView
		var roundStatus *string
		var roundEnds, settledAt *time.Time
		var by, winner *string
		var price *int64
		err := r.Scan(&p.Id, &p.Title, &p.CategoryId, &p.Spec, &p.Region, &p.Deadline, &p.Unit, &p.BaseUnitPriceIdr, &p.RefQty, &p.ThresholdQty, &p.Status,
			&p.MarketRequestedAt, &p.MarketId, &p.AuctionId, &roundStatus, &roundEnds, &settledAt, &by, &winner, &price, &p.Match)
		if roundStatus != nil {
			p.Round = &struct {
				EndsAt time.Time         `json:"endsAt"`
				Status api.AuctionStatus `json:"status"`
			}{*roundEnds, api.AuctionStatus(*roundStatus)}
		}
		if settledAt != nil {
			p.Settlement = &api.PoolSettlement{At: *settledAt, By: deref(by), Winner: deref(winner), PriceIdr: int(deref64(price))}
			p.Settlement.Lines = []poolLine{}
		}
		p.Members = []api.PoolMember{}
		return p, err
	})
	if err != nil || len(out) == 0 {
		return nonNil(out), err
	}
	ids := make([]string, len(out))
	idx := map[string]int{}
	for i, p := range out {
		ids[i], idx[p.Id] = p.Id, i
	}
	rows, err = q.Query(ctx, `
		SELECT pm.pool_id::text, pm.org_id::text = $2, coalesce(o.name, pm.name), pm.quantity, pm.opt_in, sl.id IS NOT NULL, coalesce(sl.quantity, 0),
		       coalesce(sl.share, 0), coalesce(sl.amount_idr, 0), sl.trade_id::text
		FROM pool_members pm LEFT JOIN orgs o ON o.id = pm.org_id LEFT JOIN settlement_lines sl ON sl.id = pm.settlement_line_id
		WHERE pm.pool_id::text = ANY($1) ORDER BY pm.created_at, pm.id`, ids, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var poolID, name string
		var mine, optIn, settled bool
		var qty, lineQty, share float64
		var amount int64
		var trade *string
		if err := rows.Scan(&poolID, &mine, &name, &qty, &optIn, &settled, &lineQty, &share, &amount, &trade); err != nil {
			return nil, err
		}
		p := &out[idx[poolID]]
		m := api.PoolMember{Name: name, Quantity: qty, OptIn: optIn}
		switch {
		case mine:
			m.Mine = ptr(true)
		case !optIn:
			m.Name = fmt.Sprintf("Bisnis lain #%d", len(p.Members)+1)
		}
		p.Members = append(p.Members, m)
		if settled && p.Settlement != nil {
			l := poolLine{AmountIdr: int(amount), Mine: m.Mine, Name: m.Name, OptIn: optIn, Quantity: lineQty, Share: share}
			if mine {
				l.TransactionId = trade
			}
			p.Settlement.Lines = append(p.Settlement.Lines, l)
		}
	}
	return out, rows.Err()
}

func (s *Server) ListOrgPools(ctx context.Context, req api.ListOrgPoolsRequestObject) (api.ListOrgPoolsResponseObject, error) {
	q := s.DB.Primary()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	out, err := loadPools(ctx, q, c.OrgID, `true ORDER BY p.created_at DESC, p.id`)
	if err != nil {
		return nil, err
	}
	return api.ListOrgPools200JSONResponse(out), nil
}

func poolOf(ctx context.Context, tx pgx.Tx, orgID, poolID string) (api.CollectivePool, error) {
	ps, err := loadPools(ctx, tx, orgID, `p.id::text = $1`, poolID)
	if err == nil && len(ps) == 0 {
		err = errPoolNotFound
	}
	if err != nil {
		return api.CollectivePool{}, err
	}
	return poolOnly(ps[0]), nil
}

// poolOnly drops PoolView.match.
func poolOnly(p api.PoolView) api.CollectivePool {
	return api.CollectivePool{AuctionId: p.AuctionId, BaseUnitPriceIdr: p.BaseUnitPriceIdr, CategoryId: p.CategoryId, Deadline: p.Deadline, Id: p.Id,
		MarketId: p.MarketId, MarketRequestedAt: p.MarketRequestedAt, Members: p.Members, RefQty: p.RefQty, Region: p.Region, Round: p.Round,
		Settlement: p.Settlement, Spec: p.Spec, Status: p.Status, ThresholdQty: p.ThresholdQty, Title: p.Title, Unit: p.Unit}
}

// poolLine is the element type of PoolSettlement.lines.
type poolLine = struct {
	AmountIdr     int     `json:"amountIdr"`
	Mine          *bool   `json:"mine,omitempty"`
	Name          string  `json:"name"`
	OptIn         bool    `json:"optIn"`
	Quantity      float64 `json:"quantity"`
	Share         float64 `json:"share"`
	TransactionId *string `json:"transactionId,omitempty"`
}

// lockPool locks the pool row and returns its status (404 when missing).
func lockPool(ctx context.Context, tx pgx.Tx, poolID string) (id, title, unit, status string, err error) {
	err = tx.QueryRow(ctx, `SELECT id::text, title, unit, status FROM collective_pools WHERE id::text = $1 FOR UPDATE`, poolID).Scan(&id, &title, &unit, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errPoolNotFound
	}
	return
}

var errPoolClosed = conflict("closed", "Pool sudah diproses jadi market")

func (s *Server) CreateOrgPool(ctx context.Context, req api.CreateOrgPoolRequestObject) (api.CreateOrgPoolResponseObject, error) {
	in := req.Body
	var out api.CollectivePool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("collective", "create"); err != nil {
			return err
		}
		f := map[string]string{}
		if strings.TrimSpace(in.Title) == "" {
			f["title"] = "Nama kebutuhan wajib diisi"
		}
		if strings.TrimSpace(in.Unit) == "" {
			f["unit"] = "Isi satuan"
		}
		if !in.Deadline.After(time.Now()) {
			f["deadline"] = "Deadline harus di masa depan"
		}
		if !(in.Quantity > 0) {
			f["quantity"] = "Kuantitas harus > 0"
		}
		if in.BaseUnitPriceIdr <= 0 {
			f["baseUnitPriceIdr"] = "Isi harga satuan saat ini"
		}
		if err := invalid("Periksa isian pool", f); err != nil {
			return err
		}
		region := strings.TrimSpace(deref(in.Region))
		if region == "" {
			if region, err = orgRegion(ctx, tx, c.OrgID); err != nil {
				return err
			}
		}
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO collective_pools (title, category_id, spec, region, deadline, unit, base_unit_price_idr, ref_qty, threshold_qty, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $8::numeric * 8, $9) RETURNING id::text`,
			strings.TrimSpace(in.Title), in.CategoryId, strings.TrimSpace(deref(in.Spec)), region, in.Deadline, strings.TrimSpace(in.Unit),
			in.BaseUnitPriceIdr, in.Quantity, c.sess.UserID).Scan(&id); err != nil {
			return err
		}
		if _, err := joinPool(ctx, tx, c.OrgID, id, in.Quantity, in.OptIn != nil && *in.OptIn, ""); err != nil {
			return err
		}
		if err := c.audit(ctx, tx, "Buat pool collective", "procurement", id, strings.TrimSpace(in.Title), nil); err != nil {
			return err
		}
		out, err = poolOf(ctx, tx, c.OrgID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CreateOrgPool201JSONResponse(out), nil
}

func (s *Server) JoinOrgPool(ctx context.Context, req api.JoinOrgPoolRequestObject) (api.JoinOrgPoolResponseObject, error) {
	var out api.CollectivePool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("collective", "create"); err != nil {
			return err
		}
		id, title, unit, status, err := lockPool(ctx, tx, req.PoolId)
		if err != nil {
			return err
		}
		if status != "open" {
			return errPoolClosed
		}
		qty := req.Body.Quantity
		if !(qty > 0) {
			return invalid("Isi kuantitas", map[string]string{"quantity": "Kuantitas harus > 0"})
		}
		prev, err := joinPool(ctx, tx, c.OrgID, id, qty, req.Body.OptIn != nil && *req.Body.OptIn, "")
		if err != nil {
			return err
		}
		ch := change{Field: "Demand", After: qtyLabel(qty) + " " + unit}
		action := "Gabung pool collective"
		if prev > 0 {
			before := qtyLabel(prev) + " " + unit
			ch.Before, action = &before, "Ubah demand di pool"
		}
		if err := c.audit(ctx, tx, action, "procurement", id, title, nil, ch); err != nil {
			return err
		}
		out, err = poolOf(ctx, tx, c.OrgID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.JoinOrgPool200JSONResponse(out), nil
}

func (s *Server) LeaveOrgPool(ctx context.Context, req api.LeaveOrgPoolRequestObject) (api.LeaveOrgPoolResponseObject, error) {
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("collective", "create"); err != nil {
			return err
		}
		id, title, _, status, err := lockPool(ctx, tx, req.PoolId)
		if err != nil {
			return err
		}
		if status != "open" {
			return errPoolClosed
		}
		tag, err := tx.Exec(ctx, `DELETE FROM pool_members WHERE pool_id = $1 AND org_id = $2`, id, c.OrgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return conflict("not_member", "Organisasimu belum bergabung di pool ini")
		}
		if _, err := tx.Exec(ctx, `UPDATE procurement_requests SET status = 'approved', pool_id = NULL WHERE org_id = $1 AND pool_id = $2 AND status = 'in_collective'`,
			c.OrgID, id); err != nil {
			return err
		}
		return c.audit(ctx, tx, "Keluar dari pool collective", "procurement", id, title, nil)
	})
	if err != nil {
		return nil, err
	}
	return api.LeaveOrgPool204Response{}, nil
}

func (s *Server) RequestOrgPoolMarket(ctx context.Context, req api.RequestOrgPoolMarketRequestObject) (api.RequestOrgPoolMarketResponseObject, error) {
	var out api.CollectivePool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		if err := c.need("collective", "manage"); err != nil {
			return err
		}
		id, title, _, status, err := lockPool(ctx, tx, req.PoolId)
		if err != nil {
			return err
		}
		if status != "open" {
			return conflict("closed", "Market untuk pool ini sudah diminta atau dibentuk")
		}
		var total, threshold float64
		var member bool
		if err := tx.QueryRow(ctx, `
			SELECT coalesce((SELECT sum(quantity) FROM pool_members WHERE pool_id = $1), 0), threshold_qty,
			       EXISTS (SELECT 1 FROM pool_members WHERE pool_id = $1 AND org_id = $2)
			FROM collective_pools WHERE id = $1`, id, c.OrgID).Scan(&total, &threshold, &member); err != nil {
			return err
		}
		if total < threshold {
			return conflict("not_ready", "Demand gabungan belum mencapai ambang market")
		}
		if !member {
			return conflict("not_member", "Gabung pool dulu")
		}
		if _, err := tx.Exec(ctx, `UPDATE collective_pools SET status = 'market_requested', market_requested_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		if err := c.audit(ctx, tx, "Minta market maker membentuk market", "market", id, title, nil); err != nil {
			return err
		}
		out, err = poolOf(ctx, tx, c.OrgID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.RequestOrgPoolMarket200JSONResponse(out), nil
}

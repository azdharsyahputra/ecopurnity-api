package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

func orgPartyID(ctx context.Context, q dbtx, orgID string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id::text FROM parties WHERE org_id = $1`, orgID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (l txList) VisitListOrgTransactionsResponse(w http.ResponseWriter) error {
	return l.VisitListMyTransactionsResponse(w)
}

func (s *Server) ListOrgTransactions(ctx context.Context, req api.ListOrgTransactionsRequestObject) (api.ListOrgTransactionsResponseObject, error) {
	q := s.DB.Reader()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	if err := c.need("transactions", "view"); err != nil {
		return nil, err
	}
	party, err := orgPartyID(ctx, q, c.OrgID)
	if err != nil || party == "" {
		return txList{}, err
	}
	rows, err := q.Query(ctx, txSelect+` ORDER BY t.created_at DESC LIMIT 500`, party)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := txList{}
	for rows.Next() {
		d, err := scanTx(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, txSummary{TransactionDetail: d})
	}
	return out, rows.Err()
}

func orgTxPage(ctx context.Context, q dbtx, orgID, party, tradeID string, files fileURL) (api.OrgTransactionPage, error) {
	var p api.OrgTransactionPage
	d, err := loadTransaction(ctx, q, party, tradeID, files)
	if err != nil {
		return p, err
	}
	if err := widen(d, &p); err != nil {
		return p, err
	}
	if p.Activity, err = loadAuditEntries(ctx, q, `entity_type = 'transaction' AND entity_id = $1 AND org_id = $2 ORDER BY at DESC, id DESC LIMIT 100`,
		tradeID, orgID); err != nil {
		return p, err
	}
	if d.Group == nil {
		return p, nil
	}
	pools, err := loadPools(ctx, q, orgID, `p.id::text = $1`, d.Group.Id)
	if err != nil || len(pools) == 0 || pools[0].Settlement == nil {
		return p, err
	}
	pool := pools[0]
	if err := widen(pool.Settlement, &p.Collective); err != nil {
		return p, err
	}
	p.Collective.PoolId, p.Collective.Title, p.Collective.Unit = pool.Id, pool.Title, pool.Unit
	return p, nil
}

func (s *Server) GetOrgTransaction(ctx context.Context, req api.GetOrgTransactionRequestObject) (api.GetOrgTransactionResponseObject, error) {
	q := s.DB.Reader()
	c, err := orgAccess(ctx, q, req.OrgId)
	if err != nil {
		return nil, err
	}
	if err := c.need("transactions", "view"); err != nil {
		return nil, err
	}
	party, err := orgPartyID(ctx, q, c.OrgID)
	if err != nil {
		return nil, err
	}
	p, err := orgTxPage(ctx, q, c.OrgID, party, req.Tid, s.fileURLs(ctx))
	if err != nil {
		return nil, err
	}
	return api.GetOrgTransaction200JSONResponse(p), nil
}

func (s *Server) ActOnOrgTransaction(ctx context.Context, req api.ActOnOrgTransactionRequestObject) (api.ActOnOrgTransactionResponseObject, error) {
	var p api.OrgTransactionPage
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := orgAccess(ctx, tx, req.OrgId)
		if err != nil {
			return err
		}
		party, err := orgPartyID(ctx, tx, c.OrgID)
		if err != nil {
			return err
		}
		side, err := tradeSide(ctx, tx, req.Tid, party)
		if err != nil {
			return err
		}
		if msg := txDeniedReason(c.Perms, c.Role, c.RoleLabel, string(req.Body.Action)); msg != "" {
			return &Error{Status: http.StatusForbidden, Code: "forbidden", Message: msg}
		}
		if req.Body.Action == "pay" {
			return errPayViaGateway
		}
		file, err := s.tradeFile(ctx, tx, c.sess.UserID, *req.Body)
		if err != nil {
			return err
		}
		if err := applyTradeAction(ctx, tx, req.Tid, tradeActor{Side: side, UserID: &c.sess.UserID, Name: c.actor(), OrgID: &c.OrgID, File: file}, *req.Body); err != nil {
			return err
		}
		var files fileURL
		if c.can("transactions", "view") {
			files = s.fileURLs(ctx)
		}
		p, err = orgTxPage(ctx, tx, c.OrgID, party, req.Tid, files)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.ActOnOrgTransaction200JSONResponse(p), nil
}

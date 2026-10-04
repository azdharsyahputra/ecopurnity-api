package server

import (
	"context"
	"regexp"
	"strings"

	"github.com/azdharsyahputra/ecopurnity-api/internal/api"
)

func loadUser(ctx context.Context, q dbtx, id string) (api.User, error) {
	var u api.User
	err := q.QueryRow(ctx, `
		SELECT id, name, username, email, email_verified_at IS NOT NULL, location, avatar_url, onboarded_at IS NOT NULL
		FROM users WHERE id = $1`, id).
		Scan(&u.Id, &u.Name, &u.Username, &u.Email, &u.EmailVerified, &u.Location, &u.AvatarUrl, &u.Onboarded)
	if err != nil {
		return u, err
	}
	u.Capabilities = []api.Capability{}
	rows, err := q.Query(ctx, `SELECT capability FROM user_capabilities WHERE user_id = $1 ORDER BY capability`, id)
	if err != nil {
		return u, err
	}
	for rows.Next() {
		var c api.Capability
		if err := rows.Scan(&c); err != nil {
			return u, err
		}
		u.Capabilities = append(u.Capabilities, c)
	}
	if err := rows.Err(); err != nil {
		return u, err
	}
	u.Orgs = []api.OrgMembership{}
	rows, err = q.Query(ctx, `
		SELECT o.id, o.name, m.role, coalesce(p.verification = 'verified', false)
		FROM org_members m JOIN orgs o ON o.id = m.org_id LEFT JOIN org_profiles p ON p.org_id = o.id
		WHERE m.user_id = $1 AND m.status = 'active' ORDER BY m.joined_at, o.name`, id)
	if err != nil {
		return u, err
	}
	for rows.Next() {
		var m api.OrgMembership
		if err := rows.Scan(&m.OrgId, &m.OrgName, &m.Role, &m.Verified); err != nil {
			return u, err
		}
		u.Orgs = append(u.Orgs, m)
	}
	return u, rows.Err()
}

func userParty(ctx context.Context, q dbtx, userID string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO parties (kind, user_id, name, display_kind, verified)
		SELECT 'user', u.id, u.name, 'person', u.email_verified_at IS NOT NULL FROM users u WHERE u.id = $1
		ON CONFLICT (user_id) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, userID).Scan(&id)
	return id, err
}

var notUsername = regexp.MustCompile(`[^a-z0-9._-]+`)

func usernameBase(email string) string {
	local, _, _ := strings.Cut(strings.ToLower(email), "@")
	u := strings.Trim(notUsername.ReplaceAllString(local, ""), "._-")
	if len(u) > 26 {
		u = u[:26]
	}
	for len(u) < 3 {
		u += "0"
	}
	return u
}

package usecase

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"entgo.io/ent/dialect/sql/schema"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"

	"github.com/chaitin/MonkeyCode/backend/db"
	"github.com/chaitin/MonkeyCode/backend/domain"
	"github.com/chaitin/MonkeyCode/backend/errcode"
)

func TestMemberManagerSeatLimitAndPasswords(t *testing.T) {
	ctx := context.Background()
	client, err := db.Open("sqlite3", "file:member_manager?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Schema.Create(ctx, schema.WithForeignKeys(false)); err != nil {
		t.Fatal(err)
	}
	teamID := uuid.New()
	if _, err := client.Team.Create().SetID(teamID).SetName("t").SetMemberLimit(2).Save(ctx); err != nil {
		t.Fatal(err)
	}
	m := &MemberManager{db: client, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	tu := &domain.TeamUser{Team: &domain.Team{ID: teamID}, User: &domain.User{}}

	resp, err := m.AddUserWithPassword(ctx, tu, &domain.AddTeamUserReq{Emails: []string{"A@x.com", "a@x.com", "b@x.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Users) != 2 || len(resp.Passwords) != 2 {
		t.Fatalf("users=%d passwords=%d", len(resp.Users), len(resp.Passwords))
	}
	// re-adding existing members does not consume seats
	if _, err := m.AddUser(ctx, tu, &domain.AddTeamUserReq{Emails: []string{"b@x.com"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddUser(ctx, tu, &domain.AddTeamUserReq{Emails: []string{"c@x.com"}}); err != errcode.ErrTeamMemberLimitExceeded {
		t.Fatalf("want limit exceeded, got %v", err)
	}
	if _, err := m.AddAdmin(ctx, tu, &domain.AddTeamAdminReq{Email: "boss@x.com", Name: "boss"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AutoCreateOIDCMember(ctx, teamID, &domain.OIDCExternalUser{Email: "b@x.com"}); err != nil {
		t.Fatal(err)
	}
}

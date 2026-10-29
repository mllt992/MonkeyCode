package usecase

import (
	"context"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/samber/do"

	"github.com/chaitin/MonkeyCode/backend/consts"
	"github.com/chaitin/MonkeyCode/backend/db"
	"github.com/chaitin/MonkeyCode/backend/db/teamgroup"
	"github.com/chaitin/MonkeyCode/backend/db/teamgroupmember"
	"github.com/chaitin/MonkeyCode/backend/db/teammember"
	"github.com/chaitin/MonkeyCode/backend/db/user"
	"github.com/chaitin/MonkeyCode/backend/domain"
	"github.com/chaitin/MonkeyCode/backend/errcode"
	"github.com/chaitin/MonkeyCode/backend/pkg/crypto"
	"github.com/chaitin/MonkeyCode/backend/pkg/cvt"
	"github.com/chaitin/MonkeyCode/backend/pkg/entx"
	"github.com/chaitin/MonkeyCode/backend/pkg/random"
)

const defaultMemberGroupName = "默认分组"

// MemberManager 开源版团队成员管理实现：席位上限取自 teams.member_limit，不做 license 校验。
type MemberManager struct {
	db     *db.Client
	hook   domain.TeamHook
	logger *slog.Logger
}

// NewMemberManager 创建成员管理器
func NewMemberManager(i *do.Injector) (domain.MemberManager, error) {
	m := &MemberManager{
		db:     do.MustInvoke[*db.Client](i),
		logger: do.MustInvoke[*slog.Logger](i).With("module", "usecase.member_manager"),
	}
	if hook, err := do.Invoke[domain.TeamHook](i); err == nil {
		m.hook = hook
	}
	return m, nil
}

type createdMember struct {
	user     *db.User
	password string
	isNew    bool
}

func normalizeEmails(emails []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(emails))
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

func (m *MemberManager) checkSeats(ctx context.Context, tx *db.Tx, teamID uuid.UUID, adding int) error {
	if adding <= 0 {
		return nil
	}
	team, err := tx.Team.Get(ctx, teamID)
	if err != nil {
		return err
	}
	count, err := tx.TeamMember.Query().
		Where(
			teammember.TeamIDEQ(teamID),
			teammember.RoleEQ(consts.TeamMemberRoleUser),
			teammember.HasUserWith(user.DeletedAtIsNil()),
		).Count(ctx)
	if err != nil {
		return err
	}
	if team.MemberLimit > 0 && count+adding > team.MemberLimit {
		return errcode.ErrTeamMemberLimitExceeded
	}
	return nil
}

func groupIDFor(ctx context.Context, tx *db.Tx, teamID, groupID uuid.UUID) (uuid.UUID, error) {
	if groupID != uuid.Nil {
		g, err := tx.TeamGroup.Query().Where(teamgroup.IDEQ(groupID), teamgroup.TeamIDEQ(teamID)).First(ctx)
		if err != nil {
			return uuid.Nil, err
		}
		return g.ID, nil
	}
	g, err := tx.TeamGroup.Query().Where(teamgroup.TeamIDEQ(teamID), teamgroup.NameEQ(defaultMemberGroupName)).First(ctx)
	if err == nil {
		return g.ID, nil
	}
	if !db.IsNotFound(err) {
		return uuid.Nil, err
	}
	g, err = tx.TeamGroup.Create().SetID(uuid.New()).SetTeamID(teamID).SetName(defaultMemberGroupName).Save(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	return g.ID, nil
}

// ensureMember 在事务内确保 email 对应的子账号存在、属于团队且在分组中。
func (m *MemberManager) ensureMember(ctx context.Context, tx *db.Tx, teamID, groupID uuid.UUID, email, name, avatar string) (*createdMember, error) {
	res := &createdMember{}
	u, err := tx.User.Query().
		Where(user.EmailEQ(email), user.RoleEQ(consts.UserRoleSubAccount), user.DeletedAtIsNil()).
		First(ctx)
	if err != nil && !db.IsNotFound(err) {
		return nil, err
	}
	if u == nil {
		res.password = random.String(16)
		hashed, err := crypto.HashPassword(res.password)
		if err != nil {
			return nil, errcode.ErrPasswordHashFailed
		}
		if name == "" {
			name = strings.Split(email, "@")[0]
		}
		c := tx.User.Create().
			SetID(uuid.New()).
			SetName(name).
			SetEmail(email).
			SetStatus(consts.UserStatusActive).
			SetPassword(hashed).
			SetRole(consts.UserRoleSubAccount)
		if avatar != "" {
			c.SetAvatarURL(avatar)
		}
		if u, err = c.Save(ctx); err != nil {
			return nil, err
		}
		res.isNew = true
	}
	res.user = u

	exists, err := tx.TeamMember.Query().Where(teammember.TeamIDEQ(teamID), teammember.UserIDEQ(u.ID)).Exist(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := tx.TeamMember.Create().SetID(uuid.New()).SetTeamID(teamID).SetUserID(u.ID).
			SetRole(consts.TeamMemberRoleUser).Exec(ctx); err != nil {
			return nil, err
		}
		res.isNew = true
	}
	exists, err = tx.TeamGroupMember.Query().Where(teamgroupmember.GroupIDEQ(groupID), teamgroupmember.UserIDEQ(u.ID)).Exist(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := tx.TeamGroupMember.Create().SetID(uuid.New()).SetGroupID(groupID).SetUserID(u.ID).Exec(ctx); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (m *MemberManager) countNew(ctx context.Context, tx *db.Tx, teamID uuid.UUID, emails []string) (int, error) {
	n := 0
	for _, e := range emails {
		ok, err := tx.TeamMember.Query().Where(
			teammember.TeamIDEQ(teamID),
			teammember.HasUserWith(user.EmailEQ(e), user.RoleEQ(consts.UserRoleSubAccount), user.DeletedAtIsNil()),
		).Exist(ctx)
		if err != nil {
			return 0, err
		}
		if !ok {
			n++
		}
	}
	return n, nil
}

func (m *MemberManager) addUsers(ctx context.Context, teamUser *domain.TeamUser, req *domain.AddTeamUserReq) ([]*createdMember, error) {
	teamID := teamUser.GetTeamID()
	emails := normalizeEmails(req.Emails)
	if len(emails) == 0 {
		return nil, errcode.ErrInvalidParameter
	}
	var out []*createdMember
	err := entx.WithTx2(ctx, m.db, func(tx *db.Tx) error {
		n, err := m.countNew(ctx, tx, teamID, emails)
		if err != nil {
			return err
		}
		if err := m.checkSeats(ctx, tx, teamID, n); err != nil {
			return err
		}
		gid, err := groupIDFor(ctx, tx, teamID, req.GroupID)
		if err != nil {
			return err
		}
		for _, e := range emails {
			cm, err := m.ensureMember(ctx, tx, teamID, gid, e, "", "")
			if err != nil {
				return err
			}
			out = append(out, cm)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	m.notify(ctx, teamID, out)
	return out, nil
}

func (m *MemberManager) notify(ctx context.Context, teamID uuid.UUID, members []*createdMember) {
	if m.hook == nil {
		return
	}
	for _, cm := range members {
		if cm.isNew {
			if err := m.hook.OnMemberAdded(ctx, teamID, cm.user.ID); err != nil {
				m.logger.WarnContext(ctx, "team hook failed", "error", err, "user_id", cm.user.ID)
			}
		}
	}
}

func toTeamUsers(team *domain.Team, members []*createdMember) []*domain.TeamUser {
	users := make([]*domain.TeamUser, 0, len(members))
	for _, cm := range members {
		users = append(users, &domain.TeamUser{User: cvt.From(cm.user, &domain.User{}), Team: team})
	}
	return users
}

// AddUser 添加团队成员（新账号初始密码可由管理员通过“重置密码”获取）
func (m *MemberManager) AddUser(ctx context.Context, teamUser *domain.TeamUser, req *domain.AddTeamUserReq) (*domain.AddTeamUserResp, error) {
	members, err := m.addUsers(ctx, teamUser, req)
	if err != nil {
		return nil, err
	}
	return &domain.AddTeamUserResp{Users: toTeamUsers(teamUser.Team, members)}, nil
}

// AddUserWithPassword 添加团队成员并返回新账号初始密码
func (m *MemberManager) AddUserWithPassword(ctx context.Context, teamUser *domain.TeamUser, req *domain.AddTeamUserReq) (*domain.AddTeamUserWithPasswordResp, error) {
	members, err := m.addUsers(ctx, teamUser, req)
	if err != nil {
		return nil, err
	}
	resp := &domain.AddTeamUserWithPasswordResp{Users: toTeamUsers(teamUser.Team, members)}
	for _, cm := range members {
		if cm.password != "" {
			resp.Passwords = append(resp.Passwords, &domain.TeamUserPassword{Email: cm.user.Email, Password: cm.password})
		}
	}
	return resp, nil
}

// AddAdmin 添加团队管理员（企业账号）
func (m *MemberManager) AddAdmin(ctx context.Context, teamUser *domain.TeamUser, req *domain.AddTeamAdminReq) (*domain.AddTeamAdminResp, error) {
	teamID := teamUser.GetTeamID()
	email := strings.ToLower(strings.TrimSpace(req.Email))
	password := req.Password
	generated := false
	if password == "" {
		password = random.String(16)
		generated = true
	}
	hashed, err := crypto.HashPassword(password)
	if err != nil {
		return nil, errcode.ErrPasswordHashFailed
	}
	var admin *db.User
	err = entx.WithTx2(ctx, m.db, func(tx *db.Tx) error {
		exists, err := tx.User.Query().Where(user.EmailEQ(email), user.RoleEQ(consts.UserRoleEnterprise), user.DeletedAtIsNil()).Exist(ctx)
		if err != nil {
			return err
		}
		if exists {
			return errcode.ErrUserAlreadyExists
		}
		admin, err = tx.User.Create().
			SetID(uuid.New()).
			SetName(req.Name).
			SetEmail(email).
			SetStatus(consts.UserStatusActive).
			SetPassword(hashed).
			SetRole(consts.UserRoleEnterprise).
			Save(ctx)
		if err != nil {
			return err
		}
		return tx.TeamMember.Create().SetID(uuid.New()).SetTeamID(teamID).SetUserID(admin.ID).
			SetRole(consts.TeamMemberRoleAdmin).Exec(ctx)
	})
	if err != nil {
		return nil, err
	}
	resp := &domain.AddTeamAdminResp{User: &domain.TeamUser{User: cvt.From(admin, &domain.User{}), Team: teamUser.Team}}
	if generated {
		resp.Password = password
	}
	return resp, nil
}

// AutoCreateOIDCMember OIDC 首次登录自动创建成员
func (m *MemberManager) AutoCreateOIDCMember(ctx context.Context, teamID uuid.UUID, external *domain.OIDCExternalUser) (*domain.User, error) {
	email := strings.ToLower(strings.TrimSpace(external.Email))
	if email == "" {
		return nil, errcode.ErrOIDCTeamMemberRequired
	}
	name := external.Name
	if name == "" {
		name = external.Username
	}
	var cm *createdMember
	err := entx.WithTx2(ctx, m.db, func(tx *db.Tx) error {
		n, err := m.countNew(ctx, tx, teamID, []string{email})
		if err != nil {
			return err
		}
		if err := m.checkSeats(ctx, tx, teamID, n); err != nil {
			return err
		}
		gid, err := groupIDFor(ctx, tx, teamID, uuid.Nil)
		if err != nil {
			return err
		}
		cm, err = m.ensureMember(ctx, tx, teamID, gid, email, name, external.AvatarURL)
		return err
	})
	if err != nil {
		return nil, err
	}
	m.notify(ctx, teamID, []*createdMember{cm})
	return cvt.From(cm.user, &domain.User{}), nil
}

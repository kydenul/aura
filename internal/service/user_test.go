package service

import (
	"context"
	"fmt"
	"sync"
	"testing"

	userv1 "aura/gen/proto"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mustCreate 创建一个用户，失败即终止测试，返回创建结果。
func mustCreate(t *testing.T, s *UserServer, name, email string) *userv1.User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), &userv1.CreateUserRequest{Name: name, Email: email})
	if err != nil {
		t.Fatalf("CreateUser(%q,%q) error: %v", name, email, err)
	}
	return u
}

// assertCode 断言 err 携带预期的 gRPC code。
func assertCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("err code = %v, want %v (err=%v)", status.Code(err), want, err)
	}
}

// ── CreateUser ────────────────────────────────────────────────────────────────

func TestCreateUser(t *testing.T) {
	s := NewUserServer()
	u := mustCreate(t, s, "alice", "alice@example.com")

	if u.GetId() != "u-1" {
		t.Errorf("id = %q, want u-1", u.GetId())
	}
	if u.GetName() != "alice" || u.GetEmail() != "alice@example.com" {
		t.Errorf("unexpected user: %+v", u)
	}
	if u.GetCreatedAt() == 0 {
		t.Error("created_at 应被填充")
	}

	// 第二个用户 ID 自增。
	u2 := mustCreate(t, s, "bob", "bob@example.com")
	if u2.GetId() != "u-2" {
		t.Errorf("第二个用户 id = %q, want u-2", u2.GetId())
	}
}

func TestCreateUserValidation(t *testing.T) {
	s := NewUserServer()
	cases := []struct {
		name string
		req  *userv1.CreateUserRequest
	}{
		{"空 name", &userv1.CreateUserRequest{Email: "x@example.com"}},
		{"空 email", &userv1.CreateUserRequest{Name: "x"}},
		{"全空", &userv1.CreateUserRequest{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.CreateUser(context.Background(), c.req)
			assertCode(t, err, codes.InvalidArgument)
		})
	}
}

// ── GetUser ───────────────────────────────────────────────────────────────────

func TestGetUser(t *testing.T) {
	s := NewUserServer()
	created := mustCreate(t, s, "alice", "alice@example.com")

	got, err := s.GetUser(context.Background(), &userv1.GetUserRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("GetUser error: %v", err)
	}
	if got.GetId() != created.GetId() || got.GetName() != "alice" {
		t.Errorf("unexpected user: %+v", got)
	}
}

func TestGetUserNotFound(t *testing.T) {
	s := NewUserServer()
	_, err := s.GetUser(context.Background(), &userv1.GetUserRequest{Id: "u-404"})
	assertCode(t, err, codes.NotFound)
}

// ── ListUsers ─────────────────────────────────────────────────────────────────

func TestListUsersPagination(t *testing.T) {
	s := NewUserServer()
	for i := range 25 {
		mustCreate(t, s, fmt.Sprintf("user-%d", i), fmt.Sprintf("u%d@example.com", i))
	}

	cases := []struct {
		name      string
		page      int32
		pageSize  int32
		wantCount int
	}{
		{"默认分页(page/size<=0 → 1/10)", 0, 0, 10},
		{"第二页", 2, 10, 10},
		{"第三页只剩 5 个", 3, 10, 5},
		{"超出范围返回空", 100, 10, 0},
		{"自定义页大小", 1, 5, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := s.ListUsers(context.Background(), &userv1.ListUsersRequest{Page: c.page, PageSize: c.pageSize})
			if err != nil {
				t.Fatalf("ListUsers error: %v", err)
			}
			if resp.GetTotal() != 25 {
				t.Errorf("total = %d, want 25", resp.GetTotal())
			}
			if len(resp.GetUsers()) != c.wantCount {
				t.Errorf("返回数量 = %d, want %d", len(resp.GetUsers()), c.wantCount)
			}
		})
	}
}

func TestListUsersEmpty(t *testing.T) {
	s := NewUserServer()
	resp, err := s.ListUsers(context.Background(), &userv1.ListUsersRequest{})
	if err != nil {
		t.Fatalf("ListUsers error: %v", err)
	}
	if resp.GetTotal() != 0 || len(resp.GetUsers()) != 0 {
		t.Errorf("空库应返回 total=0、空列表，got total=%d len=%d", resp.GetTotal(), len(resp.GetUsers()))
	}
}

// 之前的实现里 (page-1)*pageSize 在 int32 中可能溢出回绕成负数，触发 slice 越界 panic。
// 修复后应在大数下也不 panic 且返回空切片。
func TestListUsersPageOverflowDoesNotPanic(t *testing.T) {
	s := NewUserServer()
	mustCreate(t, s, "alice", "alice@example.com")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ListUsers 不应 panic, got: %v", r)
		}
	}()

	resp, err := s.ListUsers(context.Background(), &userv1.ListUsersRequest{
		Page:     100000,
		PageSize: 100000,
	})
	if err != nil {
		t.Fatalf("ListUsers error: %v", err)
	}
	if len(resp.GetUsers()) != 0 {
		t.Fatalf("超大 page 应返回空切片, 实际 len=%d", len(resp.GetUsers()))
	}
	if resp.GetTotal() != 1 {
		t.Fatalf("total 仍应是 1, got %d", resp.GetTotal())
	}
}

func TestListUsersPageSizeClamped(t *testing.T) {
	s := NewUserServer()
	for i := range 5 {
		mustCreate(t, s, fmt.Sprintf("u-%d", i), fmt.Sprintf("u%d@example.com", i))
	}
	// pageSize 超过上限时应被 clamp，不能借此一次性返回任意大列表。
	resp, err := s.ListUsers(context.Background(), &userv1.ListUsersRequest{
		Page:     1,
		PageSize: 1<<31 - 1,
	})
	if err != nil {
		t.Fatalf("ListUsers error: %v", err)
	}
	if len(resp.GetUsers()) != 5 {
		t.Fatalf("返回数量 = %d, want 5", len(resp.GetUsers()))
	}
}

// 跨页结果必须可重复（不能依赖 map 的随机遍历顺序）。
func TestListUsersPaginationStable(t *testing.T) {
	s := NewUserServer()
	for i := range 20 {
		mustCreate(t, s, fmt.Sprintf("u-%d", i), fmt.Sprintf("u%d@example.com", i))
	}
	first, err := s.ListUsers(context.Background(), &userv1.ListUsersRequest{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("ListUsers error: %v", err)
	}
	second, err := s.ListUsers(context.Background(), &userv1.ListUsersRequest{Page: 2, PageSize: 10})
	if err != nil {
		t.Fatalf("ListUsers error: %v", err)
	}

	// 同一份数据再请求一次 page=1，应与首次完全一致（顺序稳定）。
	firstAgain, err := s.ListUsers(context.Background(), &userv1.ListUsersRequest{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("ListUsers error: %v", err)
	}
	for i, u := range first.GetUsers() {
		if u.GetId() != firstAgain.GetUsers()[i].GetId() {
			t.Fatalf("分页结果不稳定：page=1 第 %d 个 id %q != %q", i, u.GetId(), firstAgain.GetUsers()[i].GetId())
		}
	}

	// page=1 与 page=2 的 id 集合不能重叠。
	seen := make(map[string]struct{}, 10)
	for _, u := range first.GetUsers() {
		seen[u.GetId()] = struct{}{}
	}
	for _, u := range second.GetUsers() {
		if _, dup := seen[u.GetId()]; dup {
			t.Fatalf("page 之间出现重复 id: %q", u.GetId())
		}
	}
}

// ── UpdateUser ────────────────────────────────────────────────────────────────

func TestUpdateUser(t *testing.T) {
	s := NewUserServer()
	created := mustCreate(t, s, "alice", "alice@example.com")

	t.Run("更新 name 与 email", func(t *testing.T) {
		got, err := s.UpdateUser(context.Background(), &userv1.UpdateUserRequest{
			Id:   created.GetId(),
			User: &userv1.User{Name: "alice2", Email: "alice2@example.com"},
		})
		if err != nil {
			t.Fatalf("UpdateUser error: %v", err)
		}
		if got.GetName() != "alice2" || got.GetEmail() != "alice2@example.com" {
			t.Errorf("更新未生效: %+v", got)
		}
	})

	t.Run("空字段不覆盖原值", func(t *testing.T) {
		got, err := s.UpdateUser(context.Background(), &userv1.UpdateUserRequest{
			Id:   created.GetId(),
			User: &userv1.User{Name: "alice3"}, // email 留空
		})
		if err != nil {
			t.Fatalf("UpdateUser error: %v", err)
		}
		if got.GetName() != "alice3" {
			t.Errorf("name 未更新: %+v", got)
		}
		if got.GetEmail() != "alice2@example.com" {
			t.Errorf("空 email 不应覆盖原值, got %q", got.GetEmail())
		}
	})
}

func TestUpdateUserNotFound(t *testing.T) {
	s := NewUserServer()
	_, err := s.UpdateUser(context.Background(), &userv1.UpdateUserRequest{
		Id:   "u-404",
		User: &userv1.User{Name: "x"},
	})
	assertCode(t, err, codes.NotFound)
}

func TestUpdateUserNilUser(t *testing.T) {
	s := NewUserServer()
	created := mustCreate(t, s, "alice", "alice@example.com")
	_, err := s.UpdateUser(context.Background(), &userv1.UpdateUserRequest{
		Id:   created.GetId(),
		User: nil,
	})
	assertCode(t, err, codes.InvalidArgument)
}

// 验证：之前 Get/List 返回的 User 指针不会被并发 UpdateUser 写穿。
func TestUpdateUserDoesNotMutateExistingPointers(t *testing.T) {
	s := NewUserServer()
	created := mustCreate(t, s, "alice", "alice@example.com")

	// 拿到一份 Get 的快照，记录其字段。
	snapshot, err := s.GetUser(context.Background(), &userv1.GetUserRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("GetUser error: %v", err)
	}
	originalName := snapshot.GetName()

	// 改名。
	if _, err := s.UpdateUser(context.Background(), &userv1.UpdateUserRequest{
		Id:   created.GetId(),
		User: &userv1.User{Name: "alice-renamed"},
	}); err != nil {
		t.Fatalf("UpdateUser error: %v", err)
	}

	// 之前持有的 snapshot 指针不应被 UpdateUser 改写。
	if snapshot.GetName() != originalName {
		t.Fatalf("之前 Get 返回的指针被并发 Update 改写: name=%q (期望保持 %q)",
			snapshot.GetName(), originalName)
	}

	// 再次 Get 应能拿到新值。
	got, err := s.GetUser(context.Background(), &userv1.GetUserRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("GetUser error: %v", err)
	}
	if got.GetName() != "alice-renamed" {
		t.Fatalf("更新未生效，got name=%q", got.GetName())
	}
}

// ── DeleteUser ────────────────────────────────────────────────────────────────

func TestDeleteUser(t *testing.T) {
	s := NewUserServer()
	created := mustCreate(t, s, "alice", "alice@example.com")

	if _, err := s.DeleteUser(context.Background(), &userv1.DeleteUserRequest{Id: created.GetId()}); err != nil {
		t.Fatalf("DeleteUser error: %v", err)
	}
	// 删除后再查应 NotFound。
	_, err := s.GetUser(context.Background(), &userv1.GetUserRequest{Id: created.GetId()})
	assertCode(t, err, codes.NotFound)
}

func TestDeleteUserNotFound(t *testing.T) {
	s := NewUserServer()
	_, err := s.DeleteUser(context.Background(), &userv1.DeleteUserRequest{Id: "u-404"})
	assertCode(t, err, codes.NotFound)
}

// ── 并发安全（配合 -race 运行）────────────────────────────────────────────────

func TestConcurrentCreateUsers(t *testing.T) {
	s := NewUserServer()
	const n = 100

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, err := s.CreateUser(context.Background(), &userv1.CreateUserRequest{
				Name:  fmt.Sprintf("u-%d", i),
				Email: fmt.Sprintf("u%d@example.com", i),
			})
			if err != nil {
				t.Errorf("并发 CreateUser error: %v", err)
			}
		})
	}
	wg.Wait()

	resp, err := s.ListUsers(context.Background(), &userv1.ListUsersRequest{Page: 1, PageSize: 1000})
	if err != nil {
		t.Fatalf("ListUsers error: %v", err)
	}
	if resp.GetTotal() != n {
		t.Fatalf("并发创建后 total = %d, want %d（ID 自增应无竞争）", resp.GetTotal(), n)
	}
}

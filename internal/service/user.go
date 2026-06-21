package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	userv1 "aura/gen/proto"
	"aura/pkg/log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// maxListPageSize 限制单次 ListUsers 返回的最大条数，避免被构造大 page_size 撑爆内存。
const maxListPageSize int32 = 1000

// cloneUser 返回 user 的深拷贝，避免调用方拿到 map 内对象的指针并在 Update 时被改写。
// 集中封装是为了把 proto.Clone 的类型断言收敛在一处，便于 lint/审查。
func cloneUser(u *userv1.User) *userv1.User {
	cp, _ := proto.Clone(u).(*userv1.User)
	return cp
}

// UserServer 实现 userv1.UserServiceServer 接口
// 真实项目中这里会注入 DB / Redis / 其他依赖，这里用内存 map 模拟，方便直接跑起来看效果
type UserServer struct {
	userv1.UnimplementedUserServiceServer

	mu     sync.RWMutex
	users  map[string]*userv1.User
	nextID int64
}

func NewUserServer() *UserServer {
	return &UserServer{
		users:  make(map[string]*userv1.User),
		nextID: 1,
	}
}

func (s *UserServer) CreateUser(ctx context.Context, req *userv1.CreateUserRequest) (*userv1.User, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name 不能为空")
	}
	if req.GetEmail() == "" {
		return nil, status.Error(codes.InvalidArgument, "email 不能为空")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	id := fmt.Sprintf("u-%d", s.nextID)
	s.nextID++

	user := &userv1.User{
		Id:        id,
		Name:      req.GetName(),
		Email:     req.GetEmail(),
		CreatedAt: time.Now().Unix(),
	}
	s.users[id] = user

	// 使用带 context 的日志：自动带上拦截器注入的 trace_id / request_id，便于链路追踪。
	log.InfoContext(
		ctx, "user created",
		log.String("uid", id),
		log.String("email", user.Email),
	)
	// 返回拷贝，避免调用方拿到 map 内对象的指针；后续 UpdateUser 用 COW，可减少误用面。
	return cloneUser(user), nil
}

func (s *UserServer) GetUser(_ context.Context, req *userv1.GetUserRequest) (*userv1.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.users[req.GetId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "user %s not found", req.GetId())
	}
	// 返回快照副本：调用方拿到的对象与 map 解耦，避免与并发 UpdateUser 形成读写竞争。
	return cloneUser(user), nil
}

func (s *UserServer) ListUsers(_ context.Context, req *userv1.ListUsersRequest) (*userv1.ListUsersResponse, error) {
	page := req.GetPage()
	if page <= 0 {
		page = 1
	}
	pageSize := req.GetPageSize()
	if pageSize <= 0 {
		pageSize = 10
	}
	if pageSize > maxListPageSize {
		pageSize = maxListPageSize
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// 按 id 稳定排序，保证跨页结果可重复（map 遍历顺序随机）。
	ids := make([]string, 0, len(s.users))
	for id := range s.users {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// 用 int64 做偏移计算，避免 (page-1)*pageSize 在 int32 中溢出回绕成负数。
	total64 := int64(len(s.users))
	pageSize64 := int64(pageSize)
	start := int64(page-1) * pageSize64
	end := start + pageSize64
	if start < 0 || start > total64 {
		start = total64
	}
	if end < 0 || end > total64 {
		end = total64
	}

	out := make([]*userv1.User, 0, end-start)
	for _, id := range ids[start:end] {
		// 拷贝返回，避免调用方拿到 map 内对象的指针。
		out = append(out, cloneUser(s.users[id]))
	}

	// total 可能超过 int32 上限，clamp 后返回以满足 proto 类型约束。
	var total int32
	if total64 > int64(math.MaxInt32) {
		total = math.MaxInt32
	} else {
		total = int32(total64)
	}

	return &userv1.ListUsersResponse{
		Users: out,
		Total: total,
	}, nil
}

func (s *UserServer) UpdateUser(_ context.Context, req *userv1.UpdateUserRequest) (*userv1.User, error) {
	if req.GetUser() == nil {
		return nil, status.Error(codes.InvalidArgument, "user 不能为空")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.users[req.GetId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "user %s not found", req.GetId())
	}

	// COW：基于现有对象克隆一份再改字段，最后整体替换回 map。
	// 这样 GetUser / ListUsers 之前返回的指针不会被并发改写。
	updated := cloneUser(existing)
	if name := req.GetUser().GetName(); name != "" {
		updated.Name = name
	}
	if email := req.GetUser().GetEmail(); email != "" {
		updated.Email = email
	}
	s.users[req.GetId()] = updated

	return cloneUser(updated), nil
}

func (s *UserServer) DeleteUser(_ context.Context, req *userv1.DeleteUserRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.users[req.GetId()]; !ok {
		return nil, status.Errorf(codes.NotFound, "user %s not found", req.GetId())
	}
	delete(s.users, req.GetId())
	return &emptypb.Empty{}, nil
}

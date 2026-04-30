package main

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/pipeline"
	"github.com/lufeijun/goTools/ws/server"
	"github.com/lufeijun/goTools/ws/session"
)

// UserManager 管理 userID 与 session 的映射，支持多端登录。
type UserManager struct {
	mu        sync.RWMutex
	userConns map[string][]session.Session // userID -> sessions
	connUser  map[uint64]string            // connID -> userID
}

func NewUserManager() *UserManager {
	return &UserManager{
		userConns: make(map[string][]session.Session),
		connUser:  make(map[uint64]string),
	}
}

func (um *UserManager) Bind(userID string, sess session.Session) {
	um.mu.Lock()
	defer um.mu.Unlock()
	um.userConns[userID] = append(um.userConns[userID], sess)
	um.connUser[sess.Conn().ID()] = userID
}

func (um *UserManager) Unbind(connID uint64) {
	um.mu.Lock()
	defer um.mu.Unlock()
	userID, ok := um.connUser[connID]
	if !ok {
		return
	}
	sessions := um.userConns[userID]
	for i, s := range sessions {
		if s.Conn().ID() == connID {
			um.userConns[userID] = append(sessions[:i], sessions[i+1:]...)
			break
		}
	}
	if len(um.userConns[userID]) == 0 {
		delete(um.userConns, userID)
	}
	delete(um.connUser, connID)
}

func (um *UserManager) GetSessions(userID string) []session.Session {
	um.mu.RLock()
	defer um.mu.RUnlock()
	out := make([]session.Session, len(um.userConns[userID]))
	copy(out, um.userConns[userID])
	return out
}

func (um *UserManager) SendToUser(userID string, msg *conn.Message) {
	for _, sess := range um.GetSessions(userID) {
		sess.Conn().Pipeline().FireChannelWrite(msg)
	}
}

// AuthHandler 处理第一条登录消息，认证通过后添加业务 Handler。
type AuthHandler struct {
	userManager *UserManager
	sess        session.Session
	authed      bool
}

func (h *AuthHandler) Name() string { return "auth" }

func (h *AuthHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
	if h.authed {
		ctx.FireChannelRead(msg)
		return
	}
	m, ok := msg.(*conn.Message)
	if !ok || m.Type != 0x1 {
		ctx.FireChannelRead(msg)
		return
	}

	var login struct {
		Type   string `json:"type"`
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(m.Data, &login); err != nil || login.Type != "login" || login.UserID == "" {
		log.Println("认证失败，关闭连接")
		h.sess.Close()
		return
	}

	h.authed = true
	h.userManager.Bind(login.UserID, h.sess)
	h.sess.Conn().Pipeline().AddLast("chat", &ChatHandler{
		userManager: h.userManager,
		sess:        h.sess,
		userID:      login.UserID,
	})

	log.Printf("用户 %s 登录成功，连接ID=%d", login.UserID, h.sess.Conn().ID())
}

func (h *AuthHandler) ChannelActive(ctx pipeline.Context)  { ctx.FireChannelActive() }
func (h *AuthHandler) ChannelInactive(ctx pipeline.Context) { ctx.FireChannelInactive() }
func (h *AuthHandler) ExceptionCaught(ctx pipeline.Context, err error) {}

// ChatHandler 处理聊天消息。
type ChatHandler struct {
	userManager *UserManager
	sess        session.Session
	userID      string
}

func (h *ChatHandler) Name() string { return "chat" }

func (h *ChatHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
	m, ok := msg.(*conn.Message)
	if !ok || m.Type != 0x1 {
		ctx.FireChannelRead(msg)
		return
	}

	var chatMsg struct {
		Type string `json:"type"`
		To   string `json:"to"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Data, &chatMsg); err != nil || chatMsg.Type != "chat" {
		ctx.FireChannelRead(msg)
		return
	}

	reply, _ := json.Marshal(map[string]string{
		"from": h.userID,
		"text": chatMsg.Text,
	})
	h.userManager.SendToUser(chatMsg.To, &conn.Message{
		Type: 0x1,
		Data: reply,
	})
	log.Printf("用户 %s -> %s: %s", h.userID, chatMsg.To, chatMsg.Text)

	ctx.FireChannelRead(msg)
}

func (h *ChatHandler) ChannelActive(ctx pipeline.Context)  { ctx.FireChannelActive() }
func (h *ChatHandler) ChannelInactive(ctx pipeline.Context) { ctx.FireChannelInactive() }
func (h *ChatHandler) ExceptionCaught(ctx pipeline.Context, err error) {}

func main() {
	um := NewUserManager()
	cfg := ws.Config{
		Addr:         ":8080",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	}
	srv := server.NewServer(cfg)

	srv.OnConnect(func(sess session.Session) {
		sess.Conn().Pipeline().AddLast("auth", &AuthHandler{
			userManager: um,
			sess:        sess,
		})

		// 监听状态变化，连接断开时自动清理映射
		go func(s session.Session) {
			for st := range s.StateChan() {
				if st == session.StateClosed || st == session.StateDisconnected {
					um.Unbind(s.Conn().ID())
					log.Printf("连接 %d 断开，已清理映射", s.Conn().ID())
					return
				}
			}
		}(sess)
	})

	log.Println("聊天服务器启动，监听 :8080")
	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}
}

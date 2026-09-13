package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	filemanagedb "filemanageDb"
)

type ctxKey int

const userCtxKey ctxKey = 1

// tokenTTL 访问令牌有效期。
const tokenTTL = 24 * time.Hour

func (s *server) issueToken(u *filemanagedb.User) (string, error) {
	claims := jwt.MapClaims{
		"sub":  u.ID,
		"name": u.Username,
		"role": u.Role,
		"exp":  time.Now().Add(tokenTTL).Unix(),
		"iat":  time.Now().Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(s.cfg.JWTSecret))
}

func (s *server) parseToken(ts string) (int64, error) {
	tok, err := jwt.Parse(ts, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(s.cfg.JWTSecret), nil
	})
	if err != nil || !tok.Valid {
		return 0, errors.New("invalid token")
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return 0, errors.New("invalid claims")
	}
	f, ok := claims["sub"].(float64)
	if !ok {
		return 0, errors.New("invalid subject")
	}
	return int64(f), nil
}

// auth 校验 Authorization: Bearer <token>，并把当前用户注入 context。
func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		ts, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || ts == "" {
			writeErr(w, http.StatusUnauthorized, errors.New("missing bearer token"))
			return
		}
		uid, err := s.parseToken(ts)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, errors.New("token invalid or expired"))
			return
		}
		u, err := s.store.GetUserByID(uid)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, errors.New("user not found"))
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)))
	}
}

func (s *server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil || u.Role != filemanagedb.RoleAdmin {
			writeErr(w, http.StatusForbidden, errors.New("admin privilege required"))
			return
		}
		next(w, r)
	}
}

func currentUser(r *http.Request) *filemanagedb.User {
	u, _ := r.Context().Value(userCtxKey).(*filemanagedb.User)
	return u
}

// handleLogin POST /api/v1/auth/login {username,password}
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil || req.Username == "" {
		writeErr(w, http.StatusBadRequest, errors.New("username and password required"))
		return
	}
	u, err := s.store.GetUserByName(req.Username)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, errors.New("bad username or password"))
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)); err != nil {
		writeErr(w, http.StatusUnauthorized, errors.New("bad username or password"))
		return
	}
	tok, err := s.issueToken(u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": tok,
		"user":  map[string]any{"id": u.ID, "username": u.Username, "role": u.Role, "allowedRoots": u.AllowedRoots},
	})
}

// handleRegister 首个用户自动成为管理员（引导注册）；之后注册需要管理员身份。
func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil || req.Username == "" || len(req.Password) < 6 {
		writeErr(w, http.StatusBadRequest, errors.New("username + password(>=6 chars) required"))
		return
	}
	n, err := s.store.CountUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if n > 0 {
		writeErr(w, http.StatusForbidden, errors.New("registration closed: ask an admin via /api/v1/admin/users"))
		return
	}
	u, code, err := s.createUser(req.Username, req.Password, filemanagedb.RoleAdmin, nil)
	if err != nil {
		writeErr(w, code, err)
		return
	}
	tok, _ := s.issueToken(u)
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "user": u})
}

func bcryptHash(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

func (s *server) createUser(username, password, role string, roots []string) (*filemanagedb.User, int, error) {
	hash, err := bcryptHash(password)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	u := &filemanagedb.User{Username: username, PasswordHash: string(hash), Role: role, AllowedRoots: roots}
	if err := s.store.CreateUser(u); err != nil {
		if errors.Is(err, filemanagedb.ErrExists) {
			return nil, http.StatusConflict, errors.New("username already exists")
		}
		return nil, http.StatusInternalServerError, err
	}
	u.PasswordHash = ""
	return u, http.StatusOK, nil
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": "1.0.0", "db": s.store.Driver()})
}

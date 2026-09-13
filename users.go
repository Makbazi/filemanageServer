package main

import (
	"errors"
	"net/http"
	"strconv"

	filemanagedb "filemanageDb"
)

func (s *server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if users == nil {
		users = []*filemanagedb.User{}
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username     string   `json:"username"`
		Password     string   `json:"password"`
		Role         string   `json:"role"`
		AllowedRoots []string `json:"allowedRoots"`
	}
	if err := decode(r, &req); err != nil || req.Username == "" || len(req.Password) < 6 {
		writeErr(w, http.StatusBadRequest, errors.New("username + password(>=6) required"))
		return
	}
	if req.Role != filemanagedb.RoleAdmin {
		req.Role = filemanagedb.RoleUser
	}
	u, code, err := s.createUser(req.Username, req.Password, req.Role, req.AllowedRoots)
	if err != nil {
		writeErr(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *server) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad id"))
		return
	}
	var req struct {
		Password     string   `json:"password"`
		Role         string   `json:"role"`
		AllowedRoots []string `json:"allowedRoots"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Password != "" {
		if len(req.Password) < 6 {
			writeErr(w, http.StatusBadRequest, errors.New("password too short"))
			return
		}
		hash, err := bcryptHash(req.Password)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if err := s.store.UpdateUserPassword(id, hash); err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
	}
	if req.Role != "" {
		role := filemanagedb.RoleUser
		if req.Role == filemanagedb.RoleAdmin {
			role = filemanagedb.RoleAdmin
		}
		if err := s.store.UpdateUserRole(id, role); err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
	}
	if req.AllowedRoots != nil {
		if err := s.store.UpdateUserRoots(id, req.AllowedRoots); err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad id"))
		return
	}
	if me := currentUser(r); me != nil && me.ID == id {
		writeErr(w, http.StatusBadRequest, errors.New("cannot delete yourself"))
		return
	}
	if err := s.store.DeleteUser(id); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Package state defines and sores the state for gs.
package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"time"

	"github.com/charmbracelet/log"
	"go.abhg.dev/gs/internal/git"
)

const (
	_repoJSON    = "repo"
	_branchesDir = "branches"
)

var ErrNotExist = os.ErrNotExist

// Store implements storage for gs state inside a Git repository.
type Store struct {
	b     storageBackend
	trunk string
	log   *log.Logger
}

func (s *Store) Trunk() string {
	return s.trunk
}

type InitStoreRequest struct {
	// Repository is the Git repository in which to store the state.
	Repository GitRepository

	// Trunk is the name of the trunk branch,
	// e.g. "main" or "master".
	Trunk string

	Log *log.Logger

	// Force will clear the store if it's already initialized.
	// Without this, InitStore will fail with ErrAlreadyInitialized.
	Force bool
}

type repoInfo struct {
	Trunk string `json:"trunk"`
}

var ErrAlreadyInitialized = errors.New("store already initialized")

func InitStore(ctx context.Context, req InitStoreRequest) (*Store, error) {
	logger := req.Log
	if logger == nil {
		logger = log.New(io.Discard)
	}

	if req.Trunk == "" {
		return nil, errors.New("trunk branch name is required")
	}

	b := newGitStorageBackend(req.Repository, logger)
	if err := b.Get(ctx, _repoJSON, new(repoInfo)); err == nil {
		if !req.Force {
			return nil, ErrAlreadyInitialized
		}
		if err := b.Clear(ctx, "re-initializing store"); err != nil {
			return nil, fmt.Errorf("clear store: %w", err)
		}
	}

	err := b.Update(ctx, updateRequest{
		Sets: []setRequest{
			{
				Key: _repoJSON,
				Val: repoInfo{Trunk: req.Trunk},
			},
		},
		Msg: "initialize store",
	})
	if err != nil {
		return nil, fmt.Errorf("put repo state: %w", err)
	}

	return &Store{
		b:     b,
		trunk: req.Trunk,
		log:   logger,
	}, nil
}

var ErrUninitialized = errors.New("store not initialized")

// OpenStore opens the Store for the given Git repository.
// The store will be created if it does not exist.
func OpenStore(ctx context.Context, repo GitRepository, logger *log.Logger) (*Store, error) {
	if logger == nil {
		logger = log.New(io.Discard)
	}
	b := newGitStorageBackend(repo, logger)

	var info repoInfo
	if err := b.Get(ctx, _repoJSON, &info); err != nil {
		if errors.Is(err, ErrNotExist) {
			return nil, ErrUninitialized
		}
		return nil, fmt.Errorf("get repo state: %w", err)
	}

	return &Store{
		b:     b,
		trunk: info.Trunk,
		log:   logger,
	}, nil
}

func (s *Store) branchJSON(name string) string {
	return path.Join(_branchesDir, name)
}

type branchStateBase struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

type branchState struct {
	Base branchStateBase `json:"base"`
	PR   int             `json:"pr,omitempty"`
}

type LookupResponse struct {
	Name     string
	Base     string
	BaseHash git.Hash
	PR       int
}

// Lookup returns information about a branch tracked by gs.
// If the branch is not found, [ErrNotExist] will be returned.
func (s *Store) Lookup(ctx context.Context, name string) (*LookupResponse, error) {
	var state branchState
	if err := s.b.Get(ctx, s.branchJSON(name), &state); err != nil {
		return nil, fmt.Errorf("get branch state: %w", err)
	}

	return &LookupResponse{
		Name:     name,
		Base:     state.Base.Name,
		BaseHash: git.Hash(state.Base.Hash),
		PR:       state.PR,
	}, nil
}

func (s *Store) List(ctx context.Context) ([]string, error) {
	branches, err := s.b.Keys(ctx, _branchesDir)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}

	var names []string
	for name := range branches {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

type UpsertRequest struct {
	// Name is the name of the branch.
	Name string

	// Base branch to update to.
	//
	// Leave empty to keep the current base.
	Base string

	// BaseHash is the last known hash of the base branch.
	// This is used to detect if the base branch has been updated.
	//
	// Leave empty to keep the current base hash.
	BaseHash git.Hash

	// PR is the number of the pull request associated with the branch.
	// Zero if the branch is not associated with a PR yet.
	// Leave nil to keep the current PR.
	PR *int
}

func PR(n int) *int {
	return &n
}

type UpdateRequest struct {
	Upserts []UpsertRequest
	Deletes []string
	Message string
}

func (s *Store) Update(ctx context.Context, req *UpdateRequest) error {
	if req.Message == "" {
		req.Message = fmt.Sprintf("update at %s", time.Now().Format(time.RFC3339))
	}

	sets := make([]setRequest, len(req.Upserts))
	for i, req := range req.Upserts {
		if req.Name == "" {
			return fmt.Errorf("upsert [%d]: branch name is required", i)
		}
		if req.Name == s.trunk {
			return fmt.Errorf("upsert [%d]: trunk branch is not managed by gs", i)
		}

		var b branchState
		if prev, err := s.Lookup(ctx, req.Name); err != nil {
			if !errors.Is(err, ErrNotExist) {
				return fmt.Errorf("get branch: %w", err)
			}
			// Branch does not exist yet.
			// Everything is already set to the zero value.
		} else {
			b.PR = prev.PR
			b.Base = branchStateBase{
				Name: prev.Base,
				Hash: prev.BaseHash.String(),
			}
		}

		if req.Base != "" {
			b.Base.Name = req.Base
		}
		if req.BaseHash != "" {
			b.Base.Hash = req.BaseHash.String()
		}
		if req.PR != nil {
			b.PR = *req.PR
		}

		if b.Base.Name == "" {
			return fmt.Errorf("branch %q (%d) would have no base", req.Name, i)
		}

		sets = append(sets, setRequest{
			Key: s.branchJSON(req.Name),
			Val: b,
		})
	}

	err := s.b.Update(ctx, updateRequest{
		Sets: sets,
		Dels: req.Deletes,
		Msg:  req.Message,
	})
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}

	return nil
}

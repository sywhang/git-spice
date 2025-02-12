package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/charmbracelet/log"
	"go.abhg.dev/gs/internal/git"
	"go.abhg.dev/gs/internal/spice"
	"go.abhg.dev/gs/internal/spice/state"
	"go.abhg.dev/gs/internal/text"
)

type branchFoldCmd struct {
	Branch string `placeholder:"NAME" help:"Name of the branch" predictor:"trackedBranches"`
}

func (*branchFoldCmd) Help() string {
	return text.Dedent(`
		Commits from the current branch will be merged into its base
		and the current branch will be deleted.
		Branches above the folded branch will point
		to the next branch downstack.
		Use the --branch flag to target a different branch.
	`)
}

func (cmd *branchFoldCmd) Run(ctx context.Context, log *log.Logger, opts *globalOptions) error {
	repo, store, svc, err := openRepo(ctx, log, opts)
	if err != nil {
		return err
	}

	if cmd.Branch == "" {
		currentBranch, err := repo.CurrentBranch(ctx)
		if err != nil {
			return fmt.Errorf("get current branch: %w", err)
		}
		cmd.Branch = currentBranch
	}

	if err := svc.VerifyRestacked(ctx, cmd.Branch); err != nil {
		var restackErr *spice.BranchNeedsRestackError
		switch {
		case errors.Is(err, state.ErrNotExist):
			return fmt.Errorf("branch %v not tracked", cmd.Branch)
		case errors.As(err, &restackErr):
			return fmt.Errorf("branch %v needs to be restacked before it can be folded", cmd.Branch)
		default:
			return fmt.Errorf("verify restacked: %w", err)
		}
	}

	b, err := svc.LookupBranch(ctx, cmd.Branch)
	if err != nil {
		return fmt.Errorf("get branch: %w", err)
	}

	aboves, err := svc.ListAbove(ctx, cmd.Branch)
	if err != nil {
		return fmt.Errorf("list above: %w", err)
	}

	// Merge base into current branch using a fast-forward.
	// To do this without checking out the base, we can use a local fetch
	// and fetch the feature branch "into" the base branch.
	if err := repo.Fetch(ctx, git.FetchOptions{
		Remote: ".", // local repository
		Refspecs: []git.Refspec{
			git.Refspec(cmd.Branch + ":" + b.Base),
		},
	}); err != nil {
		return fmt.Errorf("update base branch: %w", err)
	}

	newBaseHash, err := repo.PeelToCommit(ctx, b.Base)
	if err != nil {
		return fmt.Errorf("peel to commit: %w", err)
	}

	// Change the base of all branches above us
	// to the base of the branch we are folding.
	upserts := make([]state.UpsertRequest, len(aboves))
	for i, above := range aboves {
		upserts[i] = state.UpsertRequest{
			Name:     above,
			Base:     b.Base,
			BaseHash: newBaseHash,
		}
	}

	err = store.UpdateBranch(ctx, &state.UpdateRequest{
		Upserts: upserts,
		Deletes: []string{cmd.Branch},
		Message: fmt.Sprintf("folding %v into %v", cmd.Branch, b.Base),
	})
	if err != nil {
		return fmt.Errorf("upsert branches: %w", err)
	}

	// Check out base and delete the branch we are folding.
	if err := (&branchCheckoutCmd{Branch: b.Base}).Run(ctx, log, opts); err != nil {
		return fmt.Errorf("checkout base: %w", err)
	}

	if err := repo.DeleteBranch(ctx, cmd.Branch, git.BranchDeleteOptions{
		Force: true, // we know it's merged
	}); err != nil {
		return fmt.Errorf("delete branch: %w", err)
	}

	log.Infof("Branch %v has been folded into %v", cmd.Branch, b.Base)
	return nil
}

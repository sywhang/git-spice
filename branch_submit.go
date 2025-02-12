package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"go.abhg.dev/gs/internal/forge"
	"go.abhg.dev/gs/internal/git"
	"go.abhg.dev/gs/internal/must"
	"go.abhg.dev/gs/internal/secret"
	"go.abhg.dev/gs/internal/spice"
	"go.abhg.dev/gs/internal/spice/state"
	"go.abhg.dev/gs/internal/text"
	"go.abhg.dev/gs/internal/ui"
)

// submitOptions defines options that are common to all submit commands.
type submitOptions struct {
	DryRun bool `short:"n" help:"Don't actually submit the stack"`
	Fill   bool `help:"Fill in the change title and body from the commit messages"`
	// TODO: Default to Fill if --no-prompt?
	Draft     *bool `negatable:"" help:"Whether to mark change requests as drafts"`
	NoPublish bool  `name:"no-publish" help:"Push branches but don't create change requests"`

	Force bool `help:"Force push, bypassing safety checks"`

	// TODO: Other creation options e.g.:
	// - assignees
	// - labels
	// - milestone
	// - reviewers
}

const _submitHelp = `
Use --dry-run to print what would be submitted without submitting it.
For new Change Requests, a prompt will allow filling metadata.
Use --fill to populate title and body from the commit messages,
and --[no-]draft to set the draft status.
Omitting the draft flag will leave the status unchanged of open CRs.
Use --no-publish to push branches without creating CRs.
This has no effect if a branch already has an open CR.
`

type branchSubmitCmd struct {
	submitOptions

	Title string `help:"Title of the change request" placeholder:"TITLE"`
	Body  string `help:"Body of the change request" placeholder:"BODY"`

	Branch string `placeholder:"NAME" help:"Branch to submit" predictor:"trackedBranches"`
}

func (*branchSubmitCmd) Help() string {
	return text.Dedent(`
		A Change Request is created for the current branch,
		or updated if it already exists.
		Use the --branch flag to target a different branch.

		For new Change Requests, a prompt will allow filling metadata.
		Use the --title and --body flags to skip the prompt,
		or the --fill flag to use the commit message to fill them in.
		The --draft flag marks the change request as a draft.
		For updating Change Requests,
		use --draft/--no-draft to change its draft status.
		Without the flag, the draft status is not changed.

		Use --no-publish to push the branch without creating a Change
		Request.
	`)
}

func (cmd *branchSubmitCmd) Run(
	ctx context.Context,
	secretStash secret.Stash,
	log *log.Logger,
	opts *globalOptions,
) error {
	repo, store, svc, err := openRepo(ctx, log, opts)
	if err != nil {
		return err
	}

	var session submitSession
	if err := cmd.run(ctx, &session, repo, store, svc, secretStash, log, opts); err != nil {
		return err
	}

	if cmd.DryRun {
		return nil
	}

	return syncStackComments(
		ctx,
		store,
		svc,
		session.remoteRepo.Require(),
		log,
		session.branches,
	)
}

func (cmd *branchSubmitCmd) run(
	ctx context.Context,
	session *submitSession,
	repo *git.Repository,
	store *state.Store,
	svc *spice.Service,
	secretStash secret.Stash,
	log *log.Logger,
	opts *globalOptions,
) error {
	if cmd.Branch == "" {
		currentBranch, err := repo.CurrentBranch(ctx)
		if err != nil {
			return fmt.Errorf("get current branch: %w", err)
		}
		cmd.Branch = currentBranch
	}

	if cmd.Branch == store.Trunk() {
		return errors.New("cannot submit trunk")
	}

	branch, err := svc.LookupBranch(ctx, cmd.Branch)
	if err != nil {
		return fmt.Errorf("lookup branch: %w", err)
	}

	// Refuse to submit if the branch is not restacked.
	if !cmd.Force {
		if err := svc.VerifyRestacked(ctx, cmd.Branch); err != nil {
			log.Errorf("Branch %s needs to be restacked.", cmd.Branch)
			log.Errorf("Run the following command to fix this:")
			log.Errorf("  gs branch restack %s", cmd.Branch)
			log.Errorf("Or, try again with --force to submit anyway.")
			return errors.New("refusing to submit outdated branch")
			// TODO: this can be made optional with a --force or a prompt.
		}
	}

	if !cmd.DryRun && !cmd.NoPublish {
		session.branches = append(session.branches, cmd.Branch)
	}

	commitHash, err := repo.PeelToCommit(ctx, cmd.Branch)
	if err != nil {
		return fmt.Errorf("peel to commit: %w", err)
	}

	// If the branch has already been pushed to upstream with a different name,
	// use that name instead.
	// This is useful for branches that were renamed locally.
	upstreamBranch := cmd.Branch
	if branch.UpstreamBranch != "" {
		upstreamBranch = branch.UpstreamBranch
	}

	remote, err := session.remote.Get(func() (string, error) {
		return ensureRemote(ctx, repo, store, log, opts)
	})
	if err != nil {
		return err
	}

	remoteRepo, err := session.remoteRepo.Get(func() (forge.Repository, error) {
		return openRemoteRepository(ctx, log, secretStash, repo, remote)
	})
	if err != nil {
		return err
	}

	// If the branch doesn't have a CR associated with it,
	// we'll probably need to create one,
	// but verify that there isn't already one open.
	var existingChange *forge.FindChangeItem
	if branch.Change == nil {
		changes, err := remoteRepo.FindChangesByBranch(ctx, upstreamBranch, forge.FindChangesOptions{
			State: forge.ChangeOpen,
			Limit: 3,
		})
		if err != nil {
			return fmt.Errorf("list changes: %w", err)
		}

		switch len(changes) {
		case 0:
			// No PRs found, one will be created later.

		case 1:
			existingChange = changes[0]

			md, err := remoteRepo.NewChangeMetadata(ctx, existingChange.ID)
			if err != nil {
				return fmt.Errorf("get change metadata: %w", err)
			}

			// TODO: this should all happen in Service, probably.
			changeMeta, err := remoteRepo.Forge().MarshalChangeMetadata(md)
			if err != nil {
				return fmt.Errorf("marshal change metadata: %w", err)
			}

			// A CR was found, but it wasn't associated with the branch.
			// It was probably created manually.
			// We'll heal the state while we're at it.
			log.Infof("%v: Found existing CR %v", cmd.Branch, existingChange.ID)
			err = store.UpdateBranch(ctx, &state.UpdateRequest{
				Upserts: []state.UpsertRequest{
					{
						Name:           cmd.Branch,
						ChangeForge:    md.ForgeID(),
						ChangeMetadata: changeMeta,
					},
				},
				Message: fmt.Sprintf("%v: associate existing CR", cmd.Branch),
			})
			if err != nil {
				return fmt.Errorf("update state: %w", err)
			}

		default:
			// GitHub doesn't allow multiple PRs for the same branch
			// with the same base branch.
			// If we get here, it means there are multiple PRs open
			// with different base branches.
			return fmt.Errorf("multiple open change requests for %s", cmd.Branch)
			// TODO: Ask the user to pick one and associate it with the branch.
		}
	} else {
		// If a CR is already associated with the branch,
		// fetch information about it to compare with the current state.
		change, err := remoteRepo.FindChangeByID(ctx, branch.Change.ChangeID())
		if err != nil {
			return fmt.Errorf("find change: %w", err)
		}
		// TODO: If the CR is closed, we should treat it as non-existent.
		existingChange = change
	}

	// At this point, existingChange is nil only if we need to create a new CR.
	if existingChange == nil {
		if cmd.DryRun {
			if cmd.NoPublish {
				log.Infof("WOULD push branch %s", cmd.Branch)
			} else {
				log.Infof("WOULD create a CR for %s", cmd.Branch)
			}
			return nil
		}

		var prepared *preparedBranch
		if !cmd.NoPublish {
			prepared, err = cmd.preparePublish(
				ctx,
				log,
				opts,
				svc,
				store,
				repo,
				remoteRepo,
				branch.Base,
			)
			if err != nil {
				return err
			}
		}

		pushOpts := git.PushOptions{
			Remote: remote,
			Refspec: git.Refspec(
				commitHash.String() + ":refs/heads/" + upstreamBranch,
			),
			Force: cmd.Force,
		}

		// If we've already pushed this branch before,
		// we'll need a force push.
		// Use a --force-with-lease to avoid
		// overwriting someone else's changes.
		if !cmd.Force {
			existingHash, err := repo.PeelToCommit(ctx, remote+"/"+upstreamBranch)
			if err == nil {
				pushOpts.ForceWithLease = upstreamBranch + ":" + existingHash.String()
			}
		}

		err = repo.Push(ctx, pushOpts)
		if err != nil {
			return fmt.Errorf("push branch: %w", err)
		}

		// At this point, even if any other operation fails,
		// we need to save to the state that we pushed the branch
		// with the recorded name.
		upsert := state.UpsertRequest{
			Name:           cmd.Branch,
			UpstreamBranch: upstreamBranch,
		}
		defer func() {
			err := store.UpdateBranch(ctx, &state.UpdateRequest{
				Upserts: []state.UpsertRequest{upsert},
				Message: fmt.Sprintf("branch submit %s", cmd.Branch),
			})
			if err != nil {
				log.Warn("Could not update state", "error", err)
			}
		}()

		upstream := remote + "/" + cmd.Branch
		if err := repo.SetBranchUpstream(ctx, cmd.Branch, upstream); err != nil {
			log.Warn("Could not set upstream", "branch", cmd.Branch, "remote", remote, "error", err)
		}

		if prepared != nil {
			changeID, err := prepared.Publish(ctx)
			if err != nil {
				return err
			}

			changeMeta, err := remoteRepo.NewChangeMetadata(ctx, changeID)
			if err != nil {
				return fmt.Errorf("get change metadata: %w", err)
			}

			changeIDJSON, err := remoteRepo.Forge().MarshalChangeMetadata(changeMeta)
			if err != nil {
				return fmt.Errorf("marshal change ID: %w", err)
			}

			upsert.ChangeForge = changeMeta.ForgeID()
			upsert.ChangeMetadata = changeIDJSON
		} else {
			log.Infof("Pushed %s", cmd.Branch)
		}
	} else {
		if cmd.NoPublish {
			log.Warnf("Ignoring --no-publish: %s was already published: %s", cmd.Branch, existingChange.URL)
		}

		// Check base and HEAD are up-to-date.
		pull := existingChange
		var updates []string
		if pull.HeadHash != commitHash {
			updates = append(updates, "push branch")
		}
		if pull.BaseName != branch.Base {
			updates = append(updates, "set base to "+branch.Base)
		}
		if cmd.Draft != nil && pull.Draft != *cmd.Draft {
			updates = append(updates, "set draft to "+fmt.Sprint(cmd.Draft))
		}

		if len(updates) == 0 {
			log.Infof("CR %v is up-to-date: %s", pull.ID, pull.URL)
			return nil
		}

		if cmd.DryRun {
			log.Infof("WOULD update CR %v:", pull.ID)
			for _, update := range updates {
				log.Infof("  - %s", update)
			}
			return nil
		}

		if pull.HeadHash != commitHash {
			pushOpts := git.PushOptions{
				Remote: remote,
				Refspec: git.Refspec(
					commitHash.String() + ":refs/heads/" + upstreamBranch,
				),
				Force: cmd.Force,
			}
			if !cmd.Force {
				// Force push, but only if the ref is exactly
				// where we think it is.
				existingHash, err := repo.PeelToCommit(ctx, remote+"/"+upstreamBranch)
				if err == nil {
					pushOpts.ForceWithLease = upstreamBranch + ":" + existingHash.String()
				}
			}

			if err := repo.Push(ctx, pushOpts); err != nil {
				log.Error("Push failed. Branch may have been updated by someone else. Try with --force.")
				return fmt.Errorf("push branch: %w", err)
			}
		}

		if len(updates) > 0 {
			opts := forge.EditChangeOptions{
				Base:  branch.Base,
				Draft: cmd.Draft,
			}

			if err := remoteRepo.EditChange(ctx, pull.ID, opts); err != nil {
				return fmt.Errorf("edit CR %v: %w", pull.ID, err)
			}
		}

		log.Infof("Updated %v: %s", pull.ID, pull.URL)
	}

	return nil
}

type branchSubmitForm struct {
	ctx    context.Context
	svc    *spice.Service
	repo   *git.Repository
	remote forge.Repository
	log    *log.Logger

	tmpl *forge.ChangeTemplate
}

func newBranchSubmitForm(
	ctx context.Context,
	svc *spice.Service,
	repo *git.Repository,
	remoteRepo forge.Repository,
	log *log.Logger,
) *branchSubmitForm {
	return &branchSubmitForm{
		ctx:    ctx,
		svc:    svc,
		log:    log,
		repo:   repo,
		remote: remoteRepo,
	}
}

func (f *branchSubmitForm) titleField(title *string) ui.Field {
	return ui.NewInput().
		WithValue(title).
		WithTitle("Title").
		WithDescription("Short summary of the change").
		WithValidate(func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New("title cannot be blank")
			}
			return nil
		})
}

func (f *branchSubmitForm) templateField(changeTemplatesCh <-chan []*forge.ChangeTemplate) ui.Field {
	return ui.Defer(func() ui.Field {
		templates := <-changeTemplatesCh
		switch len(templates) {
		case 0:
			return nil

		case 1:
			f.tmpl = templates[0]
			return nil

		default:
			opts := make([]ui.SelectOption[*forge.ChangeTemplate], len(templates))
			for i, tmpl := range templates {
				opts[i] = ui.SelectOption[*forge.ChangeTemplate]{
					Label: tmpl.Filename,
					Value: tmpl,
				}
			}

			return ui.NewSelect[*forge.ChangeTemplate]().
				WithValue(&f.tmpl).
				WithOptions(opts...).
				WithTitle("Template").
				WithDescription("Choose a template for the change body")
		}
	})
}

func (f *branchSubmitForm) bodyField(body *string) ui.Field {
	editor := ui.Editor{
		Command: gitEditor(f.ctx, f.repo),
		Ext:     "md",
	}

	return ui.Defer(func() ui.Field {
		// By this point, the template field should have already run.
		if f.tmpl != nil {
			if *body != "" {
				*body += "\n\n"
			}
			*body += f.tmpl.Body
		}

		return ui.NewOpenEditor(editor).
			WithValue(body).
			WithTitle("Body").
			WithDescription("Open your editor to write " +
				"a detailed description of the change")
	})
}

func (f *branchSubmitForm) draftField(draft *bool) ui.Field {
	return ui.NewConfirm().
		WithValue(draft).
		WithTitle("Draft").
		WithDescription("Mark the change as a draft?")
}

// Fills change information in the branch submit command.
func (cmd *branchSubmitCmd) preparePublish(
	ctx context.Context,
	log *log.Logger,
	opts *globalOptions,
	svc *spice.Service,
	store *state.Store,
	repo *git.Repository,
	remoteRepo forge.Repository,
	baseBranch string,
) (*preparedBranch, error) {
	// Fetch the template while we're prompting the other fields.
	changeTemplatesCh := make(chan []*forge.ChangeTemplate, 1)
	go func() {
		defer close(changeTemplatesCh)

		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()

		templates, err := svc.ListChangeTemplates(ctx, remoteRepo)
		if err != nil {
			log.Warn("Could not list change templates", "error", err)
			templates = nil
		}

		changeTemplatesCh <- templates
	}()

	msgs, err := repo.CommitMessageRange(ctx, cmd.Branch, baseBranch)
	if err != nil {
		return nil, fmt.Errorf("list commits: %w", err)
	}
	if len(msgs) == 0 {
		return nil, errors.New("no commits to submit")
	}

	var (
		defaultTitle string
		defaultBody  strings.Builder
	)
	if len(msgs) == 1 {
		// If there's only one commit,
		// just the body will be the default body.
		defaultTitle = msgs[0].Subject
		defaultBody.WriteString(msgs[0].Body)
	} else {
		// Otherwise, we'll concatenate all the messages.
		// The revisions are in reverse order,
		// so we'll want to iterate in reverse.
		defaultTitle = msgs[len(msgs)-1].Subject
		for i := len(msgs) - 1; i >= 0; i-- {
			msg := msgs[i]
			if defaultBody.Len() > 0 {
				defaultBody.WriteString("\n\n")
			}
			defaultBody.WriteString(msg.Subject)
			if msg.Body != "" {
				defaultBody.WriteString("\n\n")
				defaultBody.WriteString(msg.Body)
			}
		}
	}

	var fields []ui.Field
	form := newBranchSubmitForm(ctx, svc, repo, remoteRepo, log)
	if cmd.Title == "" {
		cmd.Title = defaultTitle
		fields = append(fields, form.titleField(&cmd.Title))
	}

	if cmd.Body == "" {
		cmd.Body = defaultBody.String()
		if cmd.Fill {
			// If the user selected --fill,
			// and there are templates to choose from,
			// just pick the first template in the body.
			tmpls := <-changeTemplatesCh
			if len(tmpls) > 0 {
				cmd.Body += "\n\n" + tmpls[0].Body
			}
		} else {
			// Otherwise, we'll prompt for the template (if needed)
			// and the body.
			fields = append(fields, form.templateField(changeTemplatesCh))
			fields = append(fields, form.bodyField(&cmd.Body))
		}
	}

	// Don't mess with draft setting if we're not prompting
	// and the user didn't explicitly set it.
	if opts.Prompt && cmd.Draft == nil {
		cmd.Draft = new(bool)
		fields = append(fields, form.draftField(cmd.Draft))
	}

	// TODO: should we assume --fill if --no-prompt?
	if len(fields) > 0 && !cmd.Fill {
		if !opts.Prompt {
			return nil, fmt.Errorf("prompt for commit information: %w", errNoPrompt)
		}

		// If we're prompting and there's a prior submission attempt,
		// change the title and body to the saved values.
		prePrepared, err := store.LoadPreparedBranch(ctx, cmd.Branch)
		if err == nil && prePrepared != nil {
			usePrepared := true
			f := ui.NewConfirm().
				WithValue(&usePrepared).
				WithTitle("Recover previously filled information?").
				WithDescription(
					"We found previously filled information for this branch.\n" +
						"Would you like to recover and edit it?")
			if err := ui.Run(f); err != nil {
				return nil, fmt.Errorf("prompt for recovery: %w", err)
			}

			if usePrepared {
				cmd.Title = prePrepared.Subject
				cmd.Body = prePrepared.Body
			} else {
				// It will get cleared anyway when the branch
				// is submitted, but clear it now to avoid the
				// prompt again if this submission also fails.
				if err := store.ClearPreparedBranch(ctx, cmd.Branch); err != nil {
					log.Warn("Could not clear prepared branch information", "error", err)
				}
			}
		}

		form := ui.NewForm(fields...)
		if err := form.Run(); err != nil {
			return nil, fmt.Errorf("prompt form: %w", err)
		}
	}
	must.NotBeBlankf(cmd.Title, "CR title must have been set")

	storePrepared := state.PreparedBranch{
		Name:    cmd.Branch,
		Subject: cmd.Title,
		Body:    cmd.Body,
	}

	var draft bool
	if cmd.Draft != nil {
		draft = *cmd.Draft
	}

	if err := store.SavePreparedBranch(ctx, &storePrepared); err != nil {
		log.Warn("Could not save prepared branch. Will be unable to recover CR metadata if the push fails.", "error", err)
	}

	return &preparedBranch{
		PreparedBranch: storePrepared,
		draft:          draft,
		head:           cmd.Branch,
		base:           baseBranch,
		remoteRepo:     remoteRepo,
		store:          store,
		log:            log,
	}, nil
}

// preparedBranch is a branch that is ready to be published as a CR
// (or equivalent).
type preparedBranch struct {
	state.PreparedBranch

	head  string
	base  string
	draft bool

	remoteRepo forge.Repository
	store      *state.Store
	log        *log.Logger
}

func (b *preparedBranch) Publish(ctx context.Context) (forge.ChangeID, error) {
	result, err := b.remoteRepo.SubmitChange(ctx, forge.SubmitChangeRequest{
		Subject: b.Subject,
		Body:    b.Body,
		Head:    b.head,
		Base:    b.base,
		Draft:   b.draft,
	})
	if err != nil {
		return nil, fmt.Errorf("create change: %w", err)
	}

	if err := b.store.ClearPreparedBranch(ctx, b.Name); err != nil {
		b.log.Warn("Could not clear prepared branch", "error", err)
	}

	b.log.Infof("Created %v: %s", result.ID, result.URL)
	return result.ID, nil
}

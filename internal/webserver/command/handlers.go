package command

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"mattermost-locker/internal/announce"
	"mattermost-locker/internal/store"
	"mattermost-locker/internal/textfmt"
)

const defaultCommandName = "/lock-bot"

// Options configures the command handlers.
type Options struct {
	AuditDefaultLimit int
	AuditMaxLimit     int
}

// Handlers implements the slash command's subcommands.
type Handlers struct {
	store *store.Store
	ann   *announce.Announcer
	f     textfmt.Formatter
	opts  Options
	cmds  []subcommand
}

type subcommand struct {
	name  string
	usage string // arguments, e.g. "<lock>"
	desc  string
	run   func(ctx context.Context, req *Request, args []string) (*Response, error)
}

// New creates the command handlers.
func New(st *store.Store, ann *announce.Announcer, f textfmt.Formatter, opts Options) *Handlers {
	h := &Handlers{store: st, ann: ann, f: f, opts: opts}
	h.cmds = []subcommand{
		{"lock", "<lock>", "Lock a resource (if you already hold it, the timeout is refreshed)", h.lock},
		{"unlock", "<lock>", "Unlock a resource you hold", h.unlock},
		{"takeover", "<lock>", "Force-take a lock someone forgot to release (audited)", h.takeover},
		{"status", "", "Show the status of all locks", h.status},
		{"audit", "<lock> [-n N]", "Show recent history for a lock (default and max set in config)", h.audit},
		{"help", "", "Show this help", h.help},
	}
	return h
}

// Dispatch routes a request to the matching subcommand. Errors returned here
// are unexpected (e.g. database failures); user mistakes become ephemeral
// responses.
func (h *Handlers) Dispatch(ctx context.Context, req *Request) (*Response, error) {
	args := strings.Fields(req.Text)
	if len(args) == 0 {
		return h.help(ctx, req, nil)
	}

	name := strings.ToLower(args[0])
	for _, c := range h.cmds {
		if c.name == name {
			return c.run(ctx, req, args[1:])
		}
	}
	return Ephemeral("❌ Unknown command " + code(name) + ".\n\n" + h.usage(req)), nil
}

// ---- helpers ----

func cmdName(req *Request) string {
	if c := strings.TrimSpace(req.Command); c != "" {
		return strings.ReplaceAll(c, "`", "")
	}
	return defaultCommandName
}

// code renders user-supplied text as inline code without allowing it to break out.
func code(s string) string { return "`" + strings.ReplaceAll(s, "`", "") + "`" }

func userOf(req *Request) (store.User, error) {
	if req.UserID == "" {
		return store.User{}, errors.New("request has no user_id")
	}
	return store.User{ID: req.UserID, Name: req.UserName}, nil
}

func (h *Handlers) usage(req *Request) string {
	var b strings.Builder
	b.WriteString("**Available commands**\n")
	for _, c := range h.cmds {
		line := cmdName(req) + " " + c.name
		if c.usage != "" {
			line += " " + c.usage
		}
		fmt.Fprintf(&b, "- `%s` — %s\n", line, c.desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (h *Handlers) usageFor(req *Request, name string) string {
	for _, c := range h.cmds {
		if c.name == name {
			return strings.TrimSpace(cmdName(req) + " " + c.name + " " + c.usage)
		}
	}
	return cmdName(req)
}

func (h *Handlers) singleLockArg(req *Request, sub string, args []string) (string, *Response) {
	if len(args) != 1 {
		return "", Ephemeral("❌ Usage: `" + h.usageFor(req, sub) + "`")
	}
	return args[0], nil
}

func (h *Handlers) unknownLock(ctx context.Context, name string) (*Response, error) {
	locks, err := h.store.List(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(locks))
	for i, l := range locks {
		names[i] = l.Name
	}
	return Ephemeral(fmt.Sprintf("❌ Unknown lock %s. Available locks: %s.", code(name), strings.Join(names, ", "))), nil
}

func mention(name string) string { return "@" + name }

// ---- subcommands ----

func (h *Handlers) help(_ context.Context, req *Request, _ []string) (*Response, error) {
	return Ephemeral(h.usage(req)), nil
}

func (h *Handlers) lock(ctx context.Context, req *Request, args []string) (*Response, error) {
	name, bad := h.singleLockArg(req, "lock", args)
	if bad != nil {
		return bad, nil
	}
	user, err := userOf(req)
	if err != nil {
		return nil, err
	}

	res, err := h.store.Acquire(ctx, name, user)
	if errors.Is(err, store.ErrNotFound) {
		return h.unknownLock(ctx, name)
	}
	if err != nil {
		return nil, err
	}
	return h.lockResponse(res), nil
}

func (h *Handlers) takeover(ctx context.Context, req *Request, args []string) (*Response, error) {
	name, bad := h.singleLockArg(req, "takeover", args)
	if bad != nil {
		return bad, nil
	}
	user, err := userOf(req)
	if err != nil {
		return nil, err
	}

	res, err := h.store.Takeover(ctx, name, user)
	if errors.Is(err, store.ErrNotFound) {
		return h.unknownLock(ctx, name)
	}
	if err != nil {
		return nil, err
	}
	return h.lockResponse(res), nil
}

// lockResponse announces (where appropriate) and builds the ephemeral reply
// shared by lock and takeover.
func (h *Handlers) lockResponse(res store.LockResult) *Response {
	l := res.Lock
	switch res.Status {
	case store.StatusAcquired:
		h.ann.Locked(l)
		return Ephemeral(fmt.Sprintf("🔒 You locked **%s**. It will time out %s.", l.Name, h.f.When(l.ExpiresAt())))

	case store.StatusRefreshed:
		return Ephemeral(fmt.Sprintf("ℹ️ You already own **%s**. The timeout has been refreshed; it will now time out %s.",
			l.Name, h.f.When(l.ExpiresAt())))

	case store.StatusTookOver:
		h.ann.TakenOver(res)
		return Ephemeral(fmt.Sprintf(
			"⚠️ You took over **%s** from %s, who had locked it %s. The takeover has been announced and audited. It will now time out %s.",
			l.Name, mention(res.Previous.HolderName), h.f.When(res.Previous.LockedAt), h.f.When(l.ExpiresAt())))

	default: // StatusHeld
		return Ephemeral(fmt.Sprintf("❌ **%s** is already locked by %s, since %s. It will time out %s.",
			l.Name, mention(l.HolderName), h.f.When(l.LockedAt), h.f.When(l.ExpiresAt())))
	}
}

func (h *Handlers) unlock(ctx context.Context, req *Request, args []string) (*Response, error) {
	name, bad := h.singleLockArg(req, "unlock", args)
	if bad != nil {
		return bad, nil
	}
	user, err := userOf(req)
	if err != nil {
		return nil, err
	}

	res, err := h.store.Release(ctx, name, user)
	if errors.Is(err, store.ErrNotFound) {
		return h.unknownLock(ctx, name)
	}
	if err != nil {
		return nil, err
	}

	switch res.Status {
	case store.Unlocked:
		h.ann.Unlocked(res, user)
		return Ephemeral(fmt.Sprintf("🔓 You unlocked **%s**.", res.Lock.Name)), nil

	case store.NotLocked:
		return Ephemeral(fmt.Sprintf("ℹ️ **%s** is not locked.", res.Lock.Name)), nil

	default: // NotOwner
		l := res.Lock
		return Ephemeral(fmt.Sprintf(
			"❌ **%s** is locked by %s (since %s), so you can't unlock it.\n\n"+
				"If you're **confident the lock was forgotten** — and not intentionally left in place for a long period — "+
				"you can force it with `%s takeover %s`. Please check with %s first if you can: "+
				"taking over resets the lock's ownership and timeout, and is announced in the locks channel and recorded in the audit log.",
			l.Name, mention(l.HolderName), h.f.When(l.LockedAt),
			cmdName(req), l.Name, mention(l.HolderName))), nil
	}
}

func (h *Handlers) status(ctx context.Context, _ *Request, args []string) (*Response, error) {
	if len(args) != 0 {
		return Ephemeral("❌ `status` takes no arguments."), nil
	}
	locks, err := h.store.List(ctx)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString("**Lock status**\n\n")
	b.WriteString("| Lock | Status | Held by | Locked | Expires | Timeout |\n")
	b.WriteString("|:--|:--|:--|:--|:--|:--|\n")
	for _, l := range locks {
		if l.Locked() {
			fmt.Fprintf(&b, "| %s | 🔒 Locked | %s | %s | %s | %s |\n",
				l.Name, mention(l.HolderName), h.f.When(l.LockedAt), h.f.When(l.ExpiresAt()), textfmt.Duration(l.Timeout))
		} else {
			fmt.Fprintf(&b, "| %s | 🔓 Free | | | | %s |\n", l.Name, textfmt.Duration(l.Timeout))
		}
	}
	return Ephemeral(strings.TrimRight(b.String(), "\n")), nil
}

func (h *Handlers) audit(ctx context.Context, req *Request, args []string) (*Response, error) {
	name, n, err := parseAuditArgs(args, h.opts.AuditDefaultLimit)
	if err != nil {
		return Ephemeral("❌ " + err.Error() + "\nUsage: `" + h.usageFor(req, "audit") + "`"), nil
	}
	note := ""
	if n > h.opts.AuditMaxLimit {
		n = h.opts.AuditMaxLimit
		note = fmt.Sprintf(" (limited to the maximum of %d)", n)
	}

	canonical, entries, err := h.store.Audit(ctx, name, n)
	if errors.Is(err, store.ErrNotFound) {
		return h.unknownLock(ctx, name)
	}
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return Ephemeral(fmt.Sprintf("**Audit log for %s**: no entries yet.", canonical)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**Audit log for %s** — %d most recent, newest first%s\n\n", canonical, len(entries), note)
	b.WriteString("| When | Action | User | Details |\n")
	b.WriteString("|:--|:--|:--|:--|\n")
	for _, e := range entries {
		user := "system"
		if e.UserName != "" {
			user = mention(e.UserName)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", h.f.When(e.At), e.Action, user, h.auditDetail(e))
	}
	return Ephemeral(strings.TrimRight(b.String(), "\n")), nil
}

func (h *Handlers) auditDetail(e store.AuditEntry) string {
	switch e.Action {
	case store.ActionRelock:
		return "timeout refreshed"
	case store.ActionUnlock:
		return "held for " + textfmt.Span(e.HeldFor)
	case store.ActionTakeover:
		return fmt.Sprintf("from %s, held for %s", mention(e.PrevUserName), textfmt.Span(e.HeldFor))
	case store.ActionTimeout:
		return "released automatically after " + textfmt.Span(e.HeldFor)
	default:
		return strings.ReplaceAll(e.Detail, "|", "/")
	}
}

// parseAuditArgs parses "<lock> [-n N]" (also "-n=N" and "-nN").
func parseAuditArgs(args []string, defaultN int) (name string, n int, err error) {
	n = defaultN
	parseN := func(v string) error {
		i, convErr := strconv.Atoi(v)
		if convErr != nil || i < 1 {
			return fmt.Errorf("`-n` must be a positive whole number (got %s).", code(v))
		}
		n = i
		return nil
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n":
			if i+1 >= len(args) {
				return "", 0, errors.New("`-n` needs a number.")
			}
			i++
			if err := parseN(args[i]); err != nil {
				return "", 0, err
			}
		case strings.HasPrefix(a, "-n="):
			if err := parseN(strings.TrimPrefix(a, "-n=")); err != nil {
				return "", 0, err
			}
		case strings.HasPrefix(a, "-n") && len(a) > 2:
			if err := parseN(a[2:]); err != nil {
				return "", 0, err
			}
		case strings.HasPrefix(a, "-"):
			return "", 0, fmt.Errorf("Unknown option %s.", code(a))
		case name != "":
			return "", 0, errors.New("Only one lock name can be given.")
		default:
			name = a
		}
	}
	if name == "" {
		return "", 0, errors.New("Please give a lock name.")
	}
	return name, n, nil
}

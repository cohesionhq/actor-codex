package actor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cohesionhq/actor-codex/internal/codex"
	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

// How much of its limits each actor has used, as cohesiond shows it: the
// rate limits of the ChatGPT account Codex runs on, which every actor
// shares, and how a session's context window is counted, which the session says
// itself.

const (
	// The most plan limits, and label bytes, cohesiond takes.
	mostLimits      = 8
	longestLabel    = 64
	longestPoolID   = 256
	longestPoolName = 128
	// How long after one pull another is skipped, unless forced: it would say the same.
	pullGap = 3 * time.Second
	// How often the plan's limits are pulled, sessions running or not.
	pullEvery = 5 * time.Minute
)

// Window is one of a rate limit's windows, as Codex says it.
type Window struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins *int64   `json:"windowDurationMins"`
	// Unix seconds.
	ResetsAt *int64 `json:"resetsAt"`
}

// RateLimits is one of Codex's rate limit snapshots; an update leaves out
// what didn't change.
type RateLimits struct {
	LimitID   *string `json:"limitId"`
	LimitName *string `json:"limitName"`
	Primary   *Window `json:"primary"`
	Secondary *Window `json:"secondary"`
}

// Account is who Codex is signed in as.
type Account struct {
	Type          string  `json:"type"` // apiKey, chatgpt, amazonBedrock or gcp
	Email         *string `json:"email"`
	PlanType      *string `json:"planType"`
	WorkspaceName *string `json:"workspaceName"`
}

// HasPlan is whether `account` has plan limits: a ChatGPT sign-in, not an
// API key or a cloud provider's.
func HasPlan(account *Account) bool {
	return account != nil && account.Type == "chatgpt"
}

// PoolOf is the pool `account`'s limits belong to, which every Codex signed
// in to it shares, on any device: its email, within its workspace when it
// has one. Nil without a plan or an email, or when either is too long.
func PoolOf(account *Account) *cohesion.UsagePool {
	if !HasPlan(account) || account.Email == nil || *account.Email == "" {
		return nil
	}
	id := "codex:" + *account.Email
	if account.WorkspaceName != nil && *account.WorkspaceName != "" {
		id += "/" + *account.WorkspaceName
	}
	if len(id) > longestPoolID || len(*account.Email) > longestPoolName {
		return nil
	}
	return &cohesion.UsagePool{ID: id, Name: *account.Email}
}

// Limits are plan limits by a key of their own: the limit's id and which window.
type Limits map[string]cohesion.PlanUsage

// TakeRateLimits adds what `snapshot` says to `limits`, keeping what it leaves out.
func TakeRateLimits(limits Limits, snapshot RateLimits) {
	id := "codex"
	if snapshot.LimitID != nil && *snapshot.LimitID != "" {
		id = *snapshot.LimitID
	}
	name := ""
	if id != "codex" {
		name = id
		if snapshot.LimitName != nil && *snapshot.LimitName != "" {
			name = *snapshot.LimitName
		}
	}
	for which, window := range map[string]*Window{"primary": snapshot.Primary, "secondary": snapshot.Secondary} {
		if window == nil || window.UsedPercent == nil || math.IsNaN(*window.UsedPercent) || math.IsInf(*window.UsedPercent, 0) {
			continue
		}
		used := math.Min(1, math.Max(0, *window.UsedPercent/100))
		limits[id+"/"+which] = cohesion.PlanUsage{Label: Label(name, which, window.WindowDurationMins), Used: used, ResetsAt: window.ResetsAt}
	}
}

// Label is a limit's label: its window's length, and the limit's name when
// it isn't Codex's main one, such as "5-hour limit" or "Weekly Spark limit".
func Label(name, which string, minutes *int64) string {
	var span string
	switch {
	case minutes == nil || *minutes <= 0:
		span = map[string]string{"primary": "Short-term", "secondary": "Long-term"}[which]
	case *minutes == 7*24*60:
		span = "Weekly"
	case *minutes == 24*60:
		span = "Daily"
	case *minutes%(24*60) == 0:
		span = fmt.Sprintf("%d-day", *minutes/(24*60))
	case *minutes%60 == 0:
		span = fmt.Sprintf("%d-hour", *minutes/60)
	default:
		span = fmt.Sprintf("%d-minute", *minutes)
	}
	label := span + " limit"
	if name != "" {
		label = span + " " + strings.ReplaceAll(name, "_", " ") + " limit"
	}
	return truncate(label, longestLabel)
}

// rank orders limits: Codex's own first, primary before secondary, then the rest by key.
func rank(keys []string) {
	order := func(key string) int {
		switch key {
		case "codex/primary":
			return 0
		case "codex/secondary":
			return 1
		}
		return 2
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if a, b := order(keys[i]), order(keys[j]); a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
}

// TokenUsage is `thread/tokenUsage/updated`'s count.
type TokenUsage struct {
	Last struct {
		TotalTokens int64 `json:"totalTokens"`
	} `json:"last"`
	ModelContextWindow *int64 `json:"modelContextWindow"`
}

// ContextOf is how full the context window is after a step, or nil when Codex
// doesn't say how big it is: the last request's tokens, which the next one
// starts from.
func ContextOf(usage TokenUsage) *cohesion.ContextUsage {
	if usage.ModelContextWindow == nil || *usage.ModelContextWindow <= 0 || usage.Last.TotalTokens <= 0 {
		return nil
	}
	return &cohesion.ContextUsage{UsedTokens: max(0, usage.Last.TotalTokens), TotalTokens: *usage.ModelContextWindow}
}

func truncate(text string, bytes int) string {
	if len(text) <= bytes {
		return text
	}
	cut := text[:bytes]
	for len(cut) > 0 && !utf8Valid(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func utf8Valid(text string) bool { return strings.ToValidUTF8(text, "�") == text }

// SendUsage sends one actor's usage; it may fail, as an older cohesiond refuses it.
type SendUsage func(actor string, usage cohesion.ActorUsage) error

// Usage is the plan usage of this client's actors, said to cohesiond
// whenever it changes. Codex's rate limits are its account's, so every
// actor offered says the same ones, naming the account as their pool.
// Nothing here ever fails a session: what Codex can't say is left out.
type Usage struct {
	send   SendUsage
	actors func() []string
	log    func(string)

	mu       sync.Mutex
	plan     *bool
	pool     *cohesion.UsagePool
	limits   Limits
	said     map[string]string
	lastPull time.Time
	refused  bool
	pulling  bool
	again    *codex.Process
	running  map[*codex.Process]bool
	stop     chan struct{}
}

func NewUsage(send SendUsage, actors func() []string, log func(string)) *Usage {
	return &Usage{send: send, actors: actors, log: log, limits: Limits{}, said: map[string]string{}, running: map[*codex.Process]bool{}}
}

// Start pulls the plan's limits now and every few minutes after, until Stop.
func (u *Usage) Start() {
	u.mu.Lock()
	if u.stop != nil {
		u.mu.Unlock()
		return
	}
	u.stop = make(chan struct{})
	stop := u.stop
	u.mu.Unlock()
	go func() {
		u.tick()
		ticker := time.NewTicker(pullEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				u.tick()
			case <-stop:
				return
			}
		}
	}()
}

func (u *Usage) Stop() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stop != nil {
		close(u.stop)
		u.stop = nil
	}
}

// Track has the timer pull through `process` while it runs a session, rather
// than start a Codex of its own.
func (u *Usage) Track(process *codex.Process) {
	u.mu.Lock()
	u.running[process] = true
	u.mu.Unlock()
}

func (u *Usage) Untrack(process *codex.Process) {
	u.mu.Lock()
	delete(u.running, process)
	u.mu.Unlock()
}

// tick is one of the timer's pulls: through a session's Codex, or one started
// for it when none runs.
func (u *Usage) tick() {
	u.mu.Lock()
	var running *codex.Process
	for process := range u.running {
		running = process
		break
	}
	u.mu.Unlock()
	if running != nil {
		u.Pull(running, false)
		return
	}
	if u.recent() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	home, _ := os.UserHomeDir()
	probe, err := codex.Start(ctx, home, codex.Handler{}, func(text string) { u.log("codex: " + text) })
	if err != nil {
		u.log(fmt.Sprintf("Couldn't read the plan's limits: %v", err))
		return
	}
	defer probe.Close()
	u.Pull(probe, false)
}

func (u *Usage) recent() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.pulling || time.Since(u.lastPull) < pullGap
}

// Pull asks a running Codex for the account and its rate limits, unless a
// pull is under way or just ended. A forced pull, after a turn, always asks:
// after the one under way, if any.
func (u *Usage) Pull(process *codex.Process, force bool) {
	u.mu.Lock()
	if u.pulling {
		if force {
			u.again = process
		}
		u.mu.Unlock()
		return
	}
	if !force && time.Since(u.lastPull) < pullGap {
		u.mu.Unlock()
		return
	}
	u.pulling = true
	u.lastPull = time.Now()
	u.mu.Unlock()

	u.read(process)

	u.mu.Lock()
	u.pulling = false
	again := u.again
	u.again = nil
	u.mu.Unlock()
	if again != nil {
		u.Pull(again, true)
	}
}

func (u *Usage) read(process *codex.Process) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	u.mu.Lock()
	known := u.plan != nil
	u.mu.Unlock()
	if !known {
		var reply struct {
			Account *Account `json:"account"`
		}
		if err := process.Call(ctx, "account/read", map[string]any{}, &reply); err != nil {
			u.log(fmt.Sprintf("Couldn't read the account: %v", err))
		} else {
			u.setAccount(reply.Account)
		}
	}
	u.mu.Lock()
	noPlan := u.plan != nil && !*u.plan
	u.mu.Unlock()
	if !noPlan {
		var reply struct {
			RateLimits          *RateLimits           `json:"rateLimits"`
			RateLimitsByLimitID map[string]RateLimits `json:"rateLimitsByLimitId"`
		}
		if err := process.Call(ctx, "account/rateLimits/read", map[string]any{}, &reply); err != nil {
			u.log(fmt.Sprintf("Couldn't read the plan's limits: %v", err))
		} else {
			u.mu.Lock()
			if reply.RateLimits != nil {
				TakeRateLimits(u.limits, *reply.RateLimits)
			}
			for id, snapshot := range reply.RateLimitsByLimitID {
				if snapshot.LimitID == nil {
					snapshot.LimitID = &id
				}
				TakeRateLimits(u.limits, snapshot)
			}
			u.mu.Unlock()
		}
	}
	u.Flush()
}

// RateLimitsUpdated takes what an `account/rateLimits/updated` says, keeping
// the limits it doesn't name.
func (u *Usage) RateLimitsUpdated(params json.RawMessage) {
	var update struct {
		RateLimits *RateLimits `json:"rateLimits"`
	}
	if json.Unmarshal(params, &update) != nil || update.RateLimits == nil {
		return
	}
	u.mu.Lock()
	if u.plan != nil && !*u.plan {
		u.mu.Unlock()
		return
	}
	TakeRateLimits(u.limits, *update.RateLimits)
	u.mu.Unlock()
	u.Flush()
}

// UsageOf is what every actor says now: the plan's limits, when it has any,
// and the account's pool, once known.
func (u *Usage) UsageOf() cohesion.ActorUsage {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.usageOf()
}

func (u *Usage) usageOf() cohesion.ActorUsage {
	if u.plan != nil && !*u.plan {
		return cohesion.ActorUsage{}
	}
	keys := make([]string, 0, len(u.limits))
	for key := range u.limits {
		keys = append(keys, key)
	}
	rank(keys)
	if len(keys) > mostLimits {
		keys = keys[:mostLimits]
	}
	var plan []cohesion.PlanUsage
	for _, key := range keys {
		plan = append(plan, u.limits[key])
	}
	if len(plan) == 0 {
		return cohesion.ActorUsage{}
	}
	return cohesion.ActorUsage{Plan: plan, Pool: u.pool}
}

// Flush says each offered actor's usage that changed, forgetting the actors
// no longer offered.
func (u *Usage) Flush() {
	actors := u.actors()
	u.mu.Lock()
	for actor := range u.said {
		if !contains(actors, actor) {
			delete(u.said, actor)
		}
	}
	usage := u.usageOf()
	text, _ := json.Marshal(usage)
	var changed []string
	for _, actor := range actors {
		said, ok := u.said[actor]
		if !ok {
			said = "{}"
		}
		if said == string(text) {
			continue
		}
		u.said[actor] = string(text)
		changed = append(changed, actor)
	}
	u.mu.Unlock()
	for _, actor := range changed {
		go func(actor string) {
			if err := u.send(actor, usage); err != nil {
				u.mu.Lock()
				refused := u.refused
				u.refused = true
				u.mu.Unlock()
				// An older cohesiond refuses every one: said once.
				if !refused {
					u.log(fmt.Sprintf("Usage refused: %v", err))
				}
			}
		}(actor)
	}
}

func (u *Usage) setAccount(account *Account) {
	u.mu.Lock()
	defer u.mu.Unlock()
	plan := HasPlan(account)
	u.plan = &plan
	u.pool = PoolOf(account)
	if !plan {
		u.limits = Limits{}
	}
}

// AccountChanged forgets the account, such as after signing in again: the
// next pull reads it afresh.
func (u *Usage) AccountChanged() {
	u.mu.Lock()
	u.plan = nil
	u.pool = nil
	u.mu.Unlock()
}

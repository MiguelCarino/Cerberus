// Package flows is the Flows engine: saved recipes (a trigger plus an
// ordered list of steps that hand work to models and agent sessions),
// their runs, and the scheduler that starts them. The contract is the
// "Flows" section of docs/SCOPE.md; this package is its engine (phase 1).
//
// # Files
//
//	DATA/flows/<id>.json            a flow (0600, atomic), id "fl-<slug>"
//	DATA/flows/<id>.<version>.json  the last KeepVersions previous versions
//	DATA/flows/schedule.json        the scheduler's memory (last fire, last skip)
//	DATA/flows/runs/<run>/run.json  a run (Run), with a snapshot of its flow
//	DATA/flows/runs/<run>/steps/<n>-<step>.json   one StepRecord per finished step
//	DATA/flows/runs/<run>/files/<name>            files written by output steps
//
// # Format
//
// Flow, Trigger, Limits and Step are the stored JSON. Step is a flat
// record; each kind reads its own fields (see Step). ParseSchedule reads
// a trigger schedule (friendly forms and 5-field cron, local time, DST
// rules on Schedule). Templates and their placeholders are described on
// render (template.go); Validate lists Problems (errors block save and
// run, warnings do not).
//
// # Engine
//
// Open(Options) loads flows and runs and recovers from a restart (queued
// and running runs become failed, "daemon restarted"; waiting runs keep
// waiting). Start runs the scheduler; Close stops everything.
//
//	flows   List() []Summary, Get, Validate, Create, Update (optimistic:
//	        f.Version must match, ErrStale), SetFlowEnabled, Delete,
//	        Templates()
//	runs    RunFlow(actor, id, input), TestRun(actor, draft, input),
//	        Runs(flow, limit), GetRun(id) RunDetail, Graph(id),
//	        Cancel(actor, id), Approve(actor, id, ok, note),
//	        Continue(actor, id) (a failed, limited or canceled run goes on
//	        from its step, in the same session when the runner is a
//	        SessionResumer, with a fresh allowance of its limits),
//	        Wait(ctx, id) (until final or waiting), RunDir(id)
//	config  SetMaxRuns(n) (flows.max_runs), SetEnabled(on) (flows.enabled)
//
// Errors: ErrNotFound, ErrStale, ErrState (cancel/approve/continue in the wrong
// state), ErrDisabled, ErrClosed, *ValidationError (with Problems).
//
// Every flow change, start, approval, rejection, cancel and scheduled
// skip is written to the Auditor with the actor the caller passes
// (audit.Owner(device) from the API, "cli", ActorSystem for the
// scheduler).
//
// # Runs
//
// A run snapshots the flow, then runs steps in order on its own
// goroutine; `if` jumps forward only (a branch runs from its target to
// the end in order; jump to "end" to stop). At most MaxRuns runs execute
// at once; more wait in a FIFO queue. A run waiting at an approve step
// holds no goroutine and no slot: Approve(ok) queues it again and it
// resumes after the approve step (also after a restart, from its step
// records); Approve(!ok) ends it as rejected.
//
// Limits (Limits.Effective): max_steps counts executed steps plus fan-out
// items; max_tokens and max_cost_usd are checked after every model call
// (ModelReply usage), including the calls of sessions the run started when
// the SessionRunner is also a SessionMeter (a session that passes a limit
// is stopped); timeout_min is running time and does not count time
// spent waiting for an approval. A run that passes one ends as "limited"
// with the reason. Cancel stops a running run's step (sessions it started
// are stopped with SessionRunner.Stop) and ends it as "canceled".
//
// Step outputs: prompt → the reply text (or the parsed JSON when json is
// set; one retry on a parse error); plan → [{title, kind, task}] (one
// retry); fan_out → the list of item results (reply texts, or session
// outputs), items run with min(step max_parallel, limits.max_parallel) at
// once and the first failure cancels the rest, except a transient engine
// failure (see below), which fails only its item; session → {session, state,
// summary} (the step passes whatever the final state is: test it with an
// `if` state test); judge → {passed, gates_passed, gates_total, failed,
// session, report} (the step passes whatever the verdict: branch with an
// `if` equals test on path "passed"); if → true/false; approve → {ok,
// note, by}; output → the text, or {file, bytes} for a file.
//
// Judge steps (evals E2) judge the workspace of an earlier session step
// with an eval's criteria graph and gates (evals.ValidateCriteria rules):
// the JudgeRunner judges gate by gate, and a gate that fails with
// iterations left resumes that session with feedback written in the
// persona's voice, then judges the gate again. The judge's calls are
// charged to the run through JudgeRequest.Charge and the resumed attempts'
// usage through the SessionMeter, so the run's limits cover both; past a
// limit the judging stops, the session is stopped and the run is limited.
// An `equals` test with a path compares that field of the output.
//
// Transient engine failures: a prompt or plan step, or a fan-out item run
// as a prompt, whose model call fails with a temporary engine error (the
// local engine crashed or ran out of memory, dropped the connection, or
// the call waited too long for room: engine.IsTransient / IsTransientText
// on a *ModelError) is retried once after retryBackoff (2 s, on the Clock);
// the record keeps Retries and RetryError. Limits, validation errors and
// cancels are never retried, nor are session steps and items. While the
// engine queues a call (engine.WithWaitHook on the call's context), the
// item's Waiting and the run's Waiting say why, published (throttled to
// one event per waitGap) like any change; GetRun answers the running
// fan-out step's items as Live.
//
// # Scheduler
//
// One goroutine sleeps on a Clock timer until the earliest next due time
// (at most 15 minutes, so a changed wall clock or a suspend is noticed)
// and wakes early when a flow changes. A due flow starts with its
// input_hint as input, unless the Guard refuses new sessions or the
// previous run of the flow is still going (then it is skipped: audited
// and shown as Summary.LastSkip). Runs missed while the daemon was down
// are not caught up: one run at most starts, with a Note saying how many
// were missed. Turning a flow off and on again re-arms it from now.
//
// # Dependencies (interfaces the API wires)
//
//	ModelCaller    Complete(ctx, ModelRequest) (ModelReply, error): one chat
//	               completion through the in-process gateway; record usage
//	               with client ClientName ("cerberus-flow") and the run id as
//	               session; answer cost (existing prices; 0 for local) and
//	               energy.
//	SessionRunner  Start(ctx, SessionRequest) (id, error); Wait(ctx, id)
//	               blocks for a final state (no tight polling); Stop(id).
//	               Sessions have no parent, are tagged "flow:<flow id>" and
//	               are never auto-accepted.
//	Notifier       Notify(Notice) (desktop/push) and Post(Notice) (inbox; the
//	               item id). Approvals are Kind "flow-approve" with Link
//	               "#/flows/runs/<run>"; output steps Kind "flow".
//	Auditor        Event(actor, event, detail, targets...); *audit.Logger fits.
//	Guard          CanStart() (ok, reason): the session guard.
//	Catalog        Model(id) (known, installed); Agent(name) bool; optional
//	               PersonaCatalog (Persona(name) bool) for judge steps.
//	JudgeRunner    Judge(ctx, JudgeRequest) (JudgeResult, error): a judge
//	               step, through the eval host (internal/evals.Judge).
//	Publisher      Publish(Event{run, flow, state, step}) as EventType
//	               ("flowrun"); must not block or call back into the engine.
//	Clock          Now, NewTimer; SystemClock is the real one.
//
// # Run graph
//
// Graph(run) answers {nodes, edges, updated} with the same JSON fields as
// the session Flow view (api.FlowGraph). Nodes, in order: "input" (kind
// input; sub = the input, one line), one "st:<step id>" per step (kind
// step; label = name or id; sub = "kind · model/agent/..."; state =
// pending | running | queued | waiting | passed | failed | skipped |
// rejected | limited | canceled; href "#/s/<session>" for a session step),
// fan-out items "it:<step id>:<index>" (kind item; sub = model; state; href
// for session items), "end" (kind end; state = the run's final state), then
// "m:<model>" (kind model; href "#/models") in order of first use. Edges:
// "next" (input → first step, step → following step or end), "then" and
// "else" (an if step → its targets; "" = the following step), "item"
// (fan-out step → item; label = item kind), "ask" (prompt/plan step or
// fan-out item → model; count = calls, tokens). The graph is acyclic.
// "queued" is a running step or item whose model call the engine holds;
// detail then says why (it also notes a retried model call).
// FlowGraph(flow) is the same graph without states, for previews.
package flows

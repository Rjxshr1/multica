package main

func allTasks() []taskSpec {
	return []taskSpec{
		// Four code-reading/review tasks. These require diagnosis artifacts, not code edits.
		docTask("read-inventory-reservation", "code_reading", "easy",
			map[string]string{"README.md": "Available inventory is physical stock minus reserved units. Do not edit source.\n", "inventory.js": "export const available=(stock,reserved)=>stock+reserved;\n", "checkout.js": "import {available} from './inventory.js'; export const canCheckout=(s,r,q)=>available(s,r)>=q;\n"},
			"Read the repository without editing source. Write CODE_REVIEW.md naming the arithmetic defect, user-visible checkout risk, and smallest safe fix.", "CODE_REVIEW.md", []string{"subtract", "reserved", "checkout"}),
		docTask("read-tenant-cache-leak", "code_reading", "medium",
			map[string]string{"POLICY.md": "Cache entries must be isolated by tenant and user. Do not edit source.\n", "profile.js": "const cache=new Map(); export async function profile(tenant,user,load){ if(cache.has(user)) return cache.get(user); const value=await load(tenant,user); cache.set(user,value); return value; }\n"},
			"Review POLICY.md and profile.js. Do not edit source. Write SECURITY_REVIEW.md explaining the concrete cross-tenant failure, an exploit sequence, and the smallest keying fix.", "SECURITY_REVIEW.md", []string{"tenant", "cache", "key"}),
		docTask("read-currency-rounding", "code_reading", "medium",
			map[string]string{"CONTRACT.md": "Invoice totals are integer cents. Line unit prices are decimal dollars and quantities are integers. Do not edit source.\n", "invoice.js": "export function total(lines){ return Math.round(lines.reduce((sum,x)=>sum+x.price*x.qty,0))*100; }\n"},
			"Review the contract and implementation. Do not edit source. Write CODE_REVIEW.md identifying where rounding occurs at the wrong boundary, a counterexample, and the safe calculation.", "CODE_REVIEW.md", []string{"round", "cent", "line"}),
		docTask("read-webhook-race", "code_reading", "hard",
			map[string]string{"SYSTEM.md": "Multiple workers may deliver the same webhook. delivered_at is nullable.\n", "worker.js": "export async function deliver(db,row,http){ if(row.delivered_at) return; await http.post(row.url,row.body); await db.markDelivered(row.id); }\n"},
			"Perform a read-only concurrency review. Write RACE_REVIEW.md with an interleaving that causes duplicate external delivery, why a local mutex is insufficient, and an idempotency/claiming mitigation.", "RACE_REVIEW.md", []string{"interleav", "duplicate", "idempot"}),

		// Six concrete bug fixes with independent executable verification.
		jsFix("fix-clamp-bounds", "easy", "export function clamp(v,min,max){ return Math.min(min,Math.max(max,v)); }\n",
			"import test from 'node:test';import assert from 'node:assert/strict';import {clamp} from './index.js';test('inside',()=>assert.equal(clamp(4,0,10),4));test('bounds',()=>{assert.equal(clamp(-2,0,10),0);assert.equal(clamp(22,0,10),10)});\n",
			"Fix clamp so it respects inclusive min/max bounds. Preserve the API and run npm test.", "import {clamp} from './index.js'; if(clamp(3,3,3)!==3) throw Error('equal bounds');"),
		jsFix("fix-pagination-offset", "easy", "export function page(items,page,size){ return items.slice(page*size,(page+1)*size); }\n",
			"import test from 'node:test';import assert from 'node:assert/strict';import {page} from './index.js';test('one based',()=>assert.deepEqual(page([1,2,3,4,5],1,2),[1,2]));\n",
			"The public page number is one-based. Fix the pagination bug without changing the API and run npm test.", "import {page} from './index.js'; let a=[1,2,3,4,5]; if(JSON.stringify(page(a,3,2))!=='[5]') throw Error('last page');"),
		jsFix("fix-stable-unique", "medium", "export function unique(values){ return [...new Set(values)].sort(); }\n",
			"import test from 'node:test';import assert from 'node:assert/strict';import {unique} from './index.js';test('dedupe',()=>assert.deepEqual(unique(['b','a','b']),['b','a']));\n",
			"Fix unique so it removes duplicates while preserving first-seen order. Run npm test.", "import {unique} from './index.js'; if(JSON.stringify(unique([3,1,3,2,1]))!=='[3,1,2]') throw Error('order');"),
		jsFix("fix-memoize-arguments", "medium", "export function memoize(fn){ let ready=false,value; return (...args)=>{ if(!ready){value=fn(...args);ready=true} return value; }; }\n",
			"import test from 'node:test';import assert from 'node:assert/strict';import {memoize} from './index.js';test('per args',()=>{const f=memoize(x=>x*2);assert.equal(f(2),4);assert.equal(f(3),6)});\n",
			"Fix memoize so distinct primitive argument lists have distinct cached results and repeated lists reuse results. Run npm test.", "import {memoize} from './index.js';let n=0;let f=memoize((a,b)=>{n++;return a+b});f(1,2);f(1,2);f(2,1);if(n!==2)throw Error('cache key');"),
		jsFix("fix-date-range-inclusive", "medium", "export function days(start,end){ return Math.floor((new Date(end)-new Date(start))/86400000); }\n",
			"import test from 'node:test';import assert from 'node:assert/strict';import {days} from './index.js';test('inclusive UTC dates',()=>assert.equal(days('2026-08-01','2026-08-03'),3));\n",
			"Fix days to count inclusive UTC calendar dates. Reject an end before start. Run npm test.", "import {days} from './index.js';if(days('2026-08-03','2026-08-03')!==1)throw Error('same day');let ok=false;try{days('2026-08-04','2026-08-03')}catch{ok=true}if(!ok)throw Error('reverse');"),
		withRetryFeedback(jsFix("fix-retry-backoff", "hard", "export function delays(base,maxAttempts){ return Array.from({length:maxAttempts},(_,i)=>base*(2**(i+1))); }\n",
			"import test from 'node:test';import assert from 'node:assert/strict';import {delays} from './index.js';test('attempt schedule',()=>assert.deepEqual(delays(100,4),[100,200,400]));\n",
			"Fix delays: maxAttempts includes the initial attempt, so return delays only before retries, starting at base and doubling. Validate inputs and run npm test.", "import {delays} from './index.js';if(JSON.stringify(delays(25,1))!=='[]')throw Error('no retry');let ok=false;try{delays(0,2)}catch{ok=true}if(!ok)throw Error('base');"),
			"Hidden verification rejected the input contract: base must be a positive finite number and maxAttempts must be a positive integer. In particular, base=0 must throw. Preserve the valid retry schedule and rerun tests."),

		// Four behavior-preserving refactors with separate domains and hidden structure checks.
		refactorTask("refactor-delimited-parsers", "easy",
			"export function parseUser(s){const p=s.split('|');return {id:p[0].trim(),name:p[1].trim(),role:p[2].trim()}}\nexport function parseTeam(s){const p=s.split('|');return {id:p[0].trim(),name:p[1].trim(),role:p[2].trim()}}\n",
			"import test from 'node:test';import assert from 'node:assert/strict';import {parseUser,parseTeam} from './index.js';let e={id:'7',name:'Ada',role:'admin'};test('both',()=>{assert.deepEqual(parseUser(' 7 | Ada | admin '),e);assert.deepEqual(parseTeam(' 7 | Ada | admin '),e)});\n",
			"Refactor duplicated split/trim/mapping logic into one private boundary while preserving both exports. Run tests and write REFACTOR_NOTES.md.", ".split('|')"),
		refactorTask("refactor-email-validators", "medium",
			"export function validBuyer(x){if(typeof x!=='string')return false;const v=x.trim();return v.includes('@')&&v.length<=120}\nexport function validSeller(x){if(typeof x!=='string')return false;const v=x.trim();return v.includes('@')&&v.length<=120}\n",
			"import test from 'node:test';import assert from 'node:assert/strict';import {validBuyer,validSeller} from './index.js';test('api',()=>{for(const f of [validBuyer,validSeller]){assert.equal(f(' a@b.cn '),true);assert.equal(f(null),false)}});\n",
			"Remove duplicated validation logic without changing either exported function or behavior. Run tests and document the extracted boundary in REFACTOR_NOTES.md.", "typeof x"),
		refactorTask("refactor-money-formatters", "medium",
			"export function invoice(c){return '$'+(c/100).toFixed(2)}\nexport function refund(c){return '$'+(c/100).toFixed(2)}\n",
			"import test from 'node:test';import assert from 'node:assert/strict';import {invoice,refund} from './index.js';test('same',()=>{assert.equal(invoice(123),'$1.23');assert.equal(refund(0),'$0.00')});\n",
			"Extract the duplicated cents formatter while preserving invoice and refund exports and exact output. Run tests and write REFACTOR_NOTES.md.", ".toFixed(2)"),
		refactorTask("refactor-request-options", "hard",
			"export function getOptions(token){return {headers:{authorization:'Bearer '+token,'content-type':'application/json'},timeout:5000,retry:2}}\nexport function postOptions(token){return {headers:{authorization:'Bearer '+token,'content-type':'application/json'},timeout:5000,retry:2}}\n",
			"import test from 'node:test';import assert from 'node:assert/strict';import {getOptions,postOptions} from './index.js';test('fresh equal values',()=>{let a=getOptions('x'),b=postOptions('x');assert.deepEqual(a,b);assert.notEqual(a,b);assert.notEqual(a.headers,b.headers)});\n",
			"Remove duplicated option construction, while preserving exports and fresh-object semantics (including nested headers). Run tests and explain the boundary in REFACTOR_NOTES.md.", "content-type"),

		// Four technical-design tasks with independent rubric terms.
		docTask("design-postgres-workflow", "technical_design", "hard", map[string]string{"SYSTEM.md": "Workers poll PostgreSQL. Duplicate delivery and crashes are expected. Need bounded retries and live progress; no broker may be added.\n"},
			"Write TECHNICAL_PLAN.md under 1200 words covering state machine, atomic claim, fencing, idempotency, crash recovery, retries, transactional outbox, observability, rollout and failure tests.", "TECHNICAL_PLAN.md", []string{"state", "fenc", "idempot", "outbox", "rollback", "crash"}),
		docTask("design-webhook-delivery", "technical_design", "medium", map[string]string{"SYSTEM.md": "A SaaS sends signed customer webhooks. Endpoints can be slow or unavailable. Customers need replay and delivery audit. PostgreSQL and an existing worker pool are available.\n"},
			"Write TECHNICAL_PLAN.md for reliable webhook delivery. Cover durable events, per-endpoint ordering tradeoffs, signatures, idempotency, retry/backoff, dead letters, replay authorization, observability and rollout.", "TECHNICAL_PLAN.md", []string{"signature", "idempot", "retry", "dead", "replay", "observ"}),
		docTask("design-zero-downtime-migration", "technical_design", "hard", map[string]string{"SYSTEM.md": "A 2TB orders table needs customer_tier populated from another table. Writes continue at 20k/s. A rollback path and correctness evidence are mandatory.\n"},
			"Write MIGRATION_PLAN.md with expand/backfill/dual-read-or-write/cutover/contract phases, chunking, throttling, correctness comparison, observability, rollback and failure drills.", "MIGRATION_PLAN.md", []string{"backfill", "thrott", "compare", "cutover", "rollback", "drill"}),
		docTask("design-rate-limited-sync", "technical_design", "medium", map[string]string{"SYSTEM.md": "Sync 5M local records to a vendor limited to 100 requests/s and 10 concurrent calls. Vendor responses may time out after committing. Incremental reruns are required.\n"},
			"Write TECHNICAL_PLAN.md for the sync. Cover partitioning/checkpoints, global rate and concurrency limits, idempotency, ambiguous timeouts, retries, reconciliation, metrics and safe rollout.", "TECHNICAL_PLAN.md", []string{"checkpoint", "rate", "concurr", "idempot", "reconcil", "timeout"}),

		// Four multi-node implementation -> integration workflows.
		integrationTask("integrate-median", "medium", "export function median(v){throw Error('TODO')}\n", "import test from 'node:test';import assert from 'node:assert/strict';import {median} from './index.js';test('values',()=>{assert.equal(median([9,1,5]),5);assert.equal(median([1,9,3,7]),5)});\n", "Implement median for odd/even arrays without mutating input. Run npm test.", "import {median} from './index.js';let a=[3,1,2];median(a);if(JSON.stringify(a)!=='[3,1,2]')throw Error('mutated');"),
		withRetryFeedback(integrationTask("integrate-csv-record", "hard", "export function parseRecord(line){throw Error('TODO')}\n", "import test from 'node:test';import assert from 'node:assert/strict';import {parseRecord} from './index.js';test('quoted comma',()=>assert.deepEqual(parseRecord('7,\"Ada, Lovelace\",admin'),['7','Ada, Lovelace','admin']));\n", "Implement parseRecord for comma-delimited records with quoted commas and escaped double-quotes. Run npm test.", "import {parseRecord} from './index.js';if(JSON.stringify(parseRecord('1,\"a\"\"b\",x'))!==JSON.stringify(['1','a\"b','x']))throw Error('escaped double-quote contract');"),
			"Hidden integration verification rejected escaped quote handling: inside a quoted field, two consecutive double-quotes must decode to one literal quote. For example, the second field of 1,\"a\"\"b\",x must be a\"b. Preserve quoted-comma behavior and rerun tests."),
		integrationTask("integrate-cart-total", "medium", "export function total(lines){throw Error('TODO')}\n", "import test from 'node:test';import assert from 'node:assert/strict';import {total} from './index.js';test('cents',()=>assert.equal(total([{unitCents:199,qty:2},{unitCents:50,qty:1}]),448));\n", "Implement integer-cent cart total with validation for positive integer quantities. Run npm test.", "import {total} from './index.js';let ok=false;try{total([{unitCents:1,qty:0}])}catch{ok=true}if(!ok)throw Error('quantity');"),
		integrationTask("integrate-config-merge", "hard", "export function mergeConfig(base,override){return {...base,...override}}\n", "import test from 'node:test';import assert from 'node:assert/strict';import {mergeConfig} from './index.js';test('nested',()=>assert.deepEqual(mergeConfig({db:{host:'a',port:1},debug:false},{db:{port:2}}),{db:{host:'a',port:2},debug:false}));\n", "Implement recursive plain-object configuration merge. Arrays replace rather than merge; do not mutate inputs. Run npm test.", "import {mergeConfig} from './index.js';let a={x:{y:1},v:[1]},b={x:{z:2},v:[2]};let c=mergeConfig(a,b);c.x.y=9;if(a.x.y!==1||JSON.stringify(c.v)!=='[2]')throw Error('clone');"),

		// Four hidden-verification recovery tasks. First prompt intentionally exposes no hidden contract.
		hiddenTask("recover-empty-slug", "easy", "export function slug(v){return v.toLowerCase().trim().replaceAll(' ','-')}\n", "import test from 'node:test';import assert from 'node:assert/strict';import {slug} from './index.js';test('words',()=>assert.equal(slug('Hello World'),'hello-world'));\n", "Run visible tests and make only changes they require. Do not invent undocumented behavior.", "import {slug} from './index.js';let ok=false;try{slug('   ')}catch{ok=true}if(!ok)throw Error('empty slug must throw');", "Hidden verification rejected the result: slug must throw an Error when normalization produces an empty string. Fix the contract and run visible tests."),
		hiddenTask("recover-email-contract", "medium", "export function normalizeEmail(v){return v.trim().toLowerCase()}\n", "import test from 'node:test';import assert from 'node:assert/strict';import {normalizeEmail} from './index.js';test('normal',()=>assert.equal(normalizeEmail(' A@B.COM '),'a@b.com'));\n", "Run visible tests and make only changes they require. Do not invent undocumented behavior.", "import {normalizeEmail} from './index.js';for(const v of ['abc','a@','@b']){let ok=false;try{normalizeEmail(v)}catch{ok=true}if(!ok)throw Error('invalid email must throw')}", "Hidden verification rejected the result: normalizeEmail must throw an Error for values without non-empty local and domain parts separated by exactly one @. Implement that validation and rerun tests."),
		hiddenTask("recover-percent-range", "easy", "export function percent(v){return Math.round(v*100)+'%'}\n", "import test from 'node:test';import assert from 'node:assert/strict';import {percent} from './index.js';test('half',()=>assert.equal(percent(.5),'50%'));\n", "Run visible tests and make only changes they require. Do not invent undocumented behavior.", "import {percent} from './index.js';for(const v of [-.1,1.1,NaN]){let ok=false;try{percent(v)}catch{ok=true}if(!ok)throw Error('range must throw')}", "Hidden verification rejected the result: percent accepts only finite numbers in the inclusive [0,1] range and must throw otherwise. Fix and rerun tests."),
		hiddenTask("recover-port-bounds", "medium", "export function parsePort(v){return Number(v)}\n", "import test from 'node:test';import assert from 'node:assert/strict';import {parsePort} from './index.js';test('string',()=>assert.equal(parsePort('8080'),8080));\n", "Run visible tests and make only changes they require. Do not invent undocumented behavior.", "import {parsePort} from './index.js';for(const v of ['0','65536','1.5','abc']){let ok=false;try{parsePort(v)}catch{ok=true}if(!ok)throw Error('invalid port')}", "Hidden verification rejected the result: ports must be integer values from 1 through 65535; invalid input must throw. Fix and rerun tests."),

		// Four cross-workflow bug -> fix -> dynamic Review -> resumed integration tasks.
		crossTask("cross-format-contract", "medium", "export function format(v){return {status:'READY',value:v}}\n", "export function render(v){return format(v)}\n", "READY:42", "format must return the string READY:<value>"),
		crossTask("cross-cursor-contract", "hard", "export function nextCursor(row){return row.id}\n", "export function page(row){return {next:nextCursor(row)}}\n", "eyJpZCI6NDJ9", "cursor must be base64url JSON encoding of {id:42}"),
		crossTask("cross-money-contract", "medium", "export function price(v){return v/100}\n", "export function label(v){return '$'+price(v)}\n", "$12.30", "price must return a two-decimal dollar string for integer cents"),
		crossTask("cross-status-contract", "hard", "export function publicStatus(v){return v.toLowerCase()}\n", "export function response(v){return {status:publicStatus(v)}}\n", "in_progress", "publicStatus must map internal RUNNING to API value in_progress"),
	}
}

func docTask(id, category, difficulty string, files map[string]string, prompt, artifact string, terms []string) taskSpec {
	return taskSpec{ID: id, Category: category, Difficulty: difficulty, Mode: "single", Files: files, Stages: []stage{{ID: "review", Prompt: prompt, Verifier: verifier{RequiredFiles: map[string][]string{artifact: terms}}}}}
}

func jsFiles(source, tests string) map[string]string {
	return map[string]string{"package.json": "{\"type\":\"module\",\"scripts\":{\"test\":\"node --test\"}}\n", "index.js": source, "index.test.js": tests}
}
func nodeVerify(script string) verifier {
	return verifier{Command: []string{"node", "--input-type=module", "-e", script}}
}

func jsFix(id, difficulty, source, tests, prompt, hidden string) taskSpec {
	return taskSpec{ID: id, Category: "bug_fix", Difficulty: difficulty, Mode: "single", Files: jsFiles(source, tests), Stages: []stage{{ID: "fix", Prompt: prompt, Verifier: nodeVerify("import './index.test.js';" + hidden)}}}
}

func refactorTask(id, difficulty, source, tests, prompt, duplicatedToken string) taskSpec {
	verify := "import fs from 'node:fs';import './index.test.js';let s=fs.readFileSync('index.js','utf8');let token=" + quoteJS(duplicatedToken) + ";if(s.split(token).length-1>1)throw Error('duplication remains');"
	return taskSpec{ID: id, Category: "behavior_preserving_refactor", Difficulty: difficulty, Mode: "single", Files: jsFiles(source, tests), Stages: []stage{{ID: "refactor", Prompt: prompt, Verifier: verifier{Command: []string{"node", "--input-type=module", "-e", verify}, RequiredFiles: map[string][]string{"REFACTOR_NOTES.md": {"extract"}}}}}}
}

func integrationTask(id, difficulty, source, tests, prompt, hidden string) taskSpec {
	files := jsFiles(source, tests)
	return taskSpec{ID: id, Category: "integration", Difficulty: difficulty, Mode: "integration", Files: files, Stages: []stage{
		{ID: "implement", Prompt: prompt, Verifier: nodeVerify("import './index.test.js';" + hidden)},
		{ID: "integration", Prompt: "Act as an independent integration node. Run npm test, inspect the implementation against the task, and create INTEGRATION_PASSED.md only when it is correct.", Verifier: verifier{Command: []string{"node", "--input-type=module", "-e", "import './index.test.js';" + hidden}, RequiredFiles: map[string][]string{"INTEGRATION_PASSED.md": {"pass"}}}},
	}}
}

func withRetryFeedback(task taskSpec, feedback string) taskSpec {
	task.RetryFeedback = feedback
	return task
}

func hiddenTask(id, difficulty, source, tests, prompt, hidden, feedback string) taskSpec {
	return taskSpec{ID: id, Category: "hidden_verification_recovery", Difficulty: difficulty, Mode: "hidden_retry", Files: jsFiles(source, tests), Stages: []stage{{ID: "fix", Prompt: prompt, Verifier: nodeVerify("import './index.test.js';" + hidden)}}, RetryFeedback: feedback}
}

func crossTask(id, difficulty, dependencySource, appSource, expected, contract string) taskSpec {
	files := map[string]string{
		"package.json":        "{\"type\":\"module\",\"scripts\":{\"test\":\"node --test\"}}\n",
		"dependency.js":       dependencySource,
		"app.js":              "import {" + dependencyExport(id) + "} from './dependency.js';" + appSource,
		"integration.test.js": "import test from 'node:test';import assert from 'node:assert/strict';import {" + appExport(id) + "} from './app.js';test('contract',()=>assert.equal(" + callExpr(id) + ",'" + expected + "'));\n",
	}
	testVerifier := verifier{Command: []string{"npm", "test"}}
	finalVerifier := verifier{Command: []string{"npm", "test"}, RequiredFiles: map[string][]string{"REVIEW_APPROVED.md": {"approved"}, "INTEGRATION_PASSED.md": {"pass"}}}
	return taskSpec{ID: id, Category: "cross_workflow_dynamic_review", Difficulty: difficulty, Mode: "cross_review", Files: files, Stages: []stage{
		{ID: "integration", Prompt: "You are Workflow 1 integration. Run npm test, diagnose the dependency contract failure, and write BUG_REPORT.md for Workflow 2. Do not fix dependency.js. Required contract: " + contract + ".", Verifier: verifier{RequiredFiles: map[string][]string{"BUG_REPORT.md": {"contract"}}}},
		{ID: "fix", Prompt: "You are Workflow 2. Read BUG_REPORT.md, fix dependency.js to satisfy the stated contract, and run npm test.", Verifier: testVerifier},
		{ID: "review", Prompt: "You are a dynamically inserted independent Review node. Read BUG_REPORT.md, inspect the diff, run npm test, and create REVIEW_APPROVED.md only if the contract is met.", Verifier: verifier{Command: []string{"npm", "test"}, RequiredFiles: map[string][]string{"REVIEW_APPROVED.md": {"approved"}}}},
		{ID: "integration", Prompt: "You are Workflow 1 resumed integration. Run npm test and create INTEGRATION_PASSED.md only if both tests and REVIEW_APPROVED.md exist.", Verifier: finalVerifier},
	}}
}

func dependencyExport(id string) string {
	switch id {
	case "cross-format-contract":
		return "format"
	case "cross-cursor-contract":
		return "nextCursor"
	case "cross-money-contract":
		return "price"
	default:
		return "publicStatus"
	}
}
func appExport(id string) string {
	switch id {
	case "cross-format-contract":
		return "render"
	case "cross-cursor-contract":
		return "page"
	case "cross-money-contract":
		return "label"
	default:
		return "response"
	}
}
func callExpr(id string) string {
	switch id {
	case "cross-format-contract":
		return "render(42)"
	case "cross-cursor-contract":
		return "page({id:42}).next"
	case "cross-money-contract":
		return "label(1230)"
	default:
		return "response('RUNNING').status"
	}
}
func quoteJS(value string) string {
	out := "'"
	for _, r := range value {
		if r == '\\' || r == '\'' {
			out += "\\"
		}
		out += string(r)
	}
	return out + "'"
}

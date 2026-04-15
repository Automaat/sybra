export namespace agent {

	export class Agent {
	    id: string;
	    taskId: string;
	    mode: string;
	    state: string;
	    sessionId: string;
	    costUsd: number;
	    inputTokens?: number;
	    outputTokens?: number;
	    // Go type: time
	    startedAt: any;
	    // Go type: time
	    lastEventAt: any;
	    logPath?: string;
	    external: boolean;
	    pid?: number;
	    command?: string;
	    name?: string;
	    project?: string;
	    provider?: string;
	    model?: string;
	    turnCount?: number;
	    escalationReason?: string;
	    errorKind?: string;
	    errorMsg?: string;
	    awaitingApproval?: boolean;

	    static createFrom(source: any = {}) {
	        return new Agent(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.taskId = source["taskId"];
	        this.mode = source["mode"];
	        this.state = source["state"];
	        this.sessionId = source["sessionId"];
	        this.costUsd = source["costUsd"];
	        this.inputTokens = source["inputTokens"];
	        this.outputTokens = source["outputTokens"];
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.lastEventAt = this.convertValues(source["lastEventAt"], null);
	        this.logPath = source["logPath"];
	        this.external = source["external"];
	        this.pid = source["pid"];
	        this.command = source["command"];
	        this.name = source["name"];
	        this.project = source["project"];
	        this.provider = source["provider"];
	        this.model = source["model"];
	        this.turnCount = source["turnCount"];
	        this.escalationReason = source["escalationReason"];
	        this.errorKind = source["errorKind"];
	        this.errorMsg = source["errorMsg"];
	        this.awaitingApproval = source["awaitingApproval"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ToolResultBlock {
	    toolUseId: string;
	    content: string;
	    isError?: boolean;

	    static createFrom(source: any = {}) {
	        return new ToolResultBlock(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.toolUseId = source["toolUseId"];
	        this.content = source["content"];
	        this.isError = source["isError"];
	    }
	}
	export class ToolUseBlock {
	    id: string;
	    name: string;
	    input: Record<string, any>;

	    static createFrom(source: any = {}) {
	        return new ToolUseBlock(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.input = source["input"];
	    }
	}
	export class ConvoEvent {
	    type: string;
	    subtype?: string;
	    sessionId?: string;
	    text?: string;
	    toolUses?: ToolUseBlock[];
	    toolResults?: ToolResultBlock[];
	    costUsd?: number;
	    inputTokens?: number;
	    outputTokens?: number;
	    isPartial?: boolean;
	    // Go type: time
	    timestamp: any;
	    raw?: number[];
	    errorType?: string;
	    errorStatus?: number;

	    static createFrom(source: any = {}) {
	        return new ConvoEvent(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.subtype = source["subtype"];
	        this.sessionId = source["sessionId"];
	        this.text = source["text"];
	        this.toolUses = this.convertValues(source["toolUses"], ToolUseBlock);
	        this.toolResults = this.convertValues(source["toolResults"], ToolResultBlock);
	        this.costUsd = source["costUsd"];
	        this.inputTokens = source["inputTokens"];
	        this.outputTokens = source["outputTokens"];
	        this.isPartial = source["isPartial"];
	        this.timestamp = this.convertValues(source["timestamp"], null);
	        this.raw = source["raw"];
	        this.errorType = source["errorType"];
	        this.errorStatus = source["errorStatus"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class PlanStep {
	    content: string;
	    status: string;
	
	    static createFrom(source: any = {}) {
	        return new PlanStep(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.content = source["content"];
	        this.status = source["status"];
	    }
	}
	export class StreamEvent {
	    type: string;
	    content?: string;
	    session_id?: string;
	    cost_usd?: number;
	    input_tokens?: number;
	    output_tokens?: number;
	    subtype?: string;
	    // Go type: time
	    timestamp: any;
	    error_type?: string;
	    error_status?: number;
	    plan_steps?: PlanStep[];
	
	    static createFrom(source: any = {}) {
	        return new StreamEvent(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.content = source["content"];
	        this.session_id = source["session_id"];
	        this.cost_usd = source["cost_usd"];
	        this.input_tokens = source["input_tokens"];
	        this.output_tokens = source["output_tokens"];
	        this.subtype = source["subtype"];
	        this.timestamp = this.convertValues(source["timestamp"], null);
	        this.error_type = source["error_type"];
	        this.error_status = source["error_status"];
	        this.plan_steps = this.convertValues(source["plan_steps"], PlanStep);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}


}

export namespace config {

	export class AgentDefaults {
	    provider: string;
	    model: string;
	    mode: string;
	    maxConcurrent: number;
	    researchMachineDir: string;
	    maxCostUsd: number;
	    maxTurns: number;
	    requirePermissions?: boolean;
	    maxLogEvents: number;

	    static createFrom(source: any = {}) {
	        return new AgentDefaults(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.provider = source["provider"];
	        this.model = source["model"];
	        this.mode = source["mode"];
	        this.maxConcurrent = source["maxConcurrent"];
	        this.researchMachineDir = source["researchMachineDir"];
	        this.maxCostUsd = source["maxCostUsd"];
	        this.maxTurns = source["maxTurns"];
	        this.requirePermissions = source["requirePermissions"];
	        this.maxLogEvents = source["maxLogEvents"];
	    }
	}
	export class AuditConfig {
	    enabled: boolean;
	    retentionDays: number;

	    static createFrom(source: any = {}) {
	        return new AuditConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.retentionDays = source["retentionDays"];
	    }
	}
	export class NotificationConfig {
	    desktop: boolean;

	    static createFrom(source: any = {}) {
	        return new NotificationConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.desktop = source["desktop"];
	    }
	}
	export class OrchestratorConfig {
	    autoTriage: boolean;
	    autoPlan: boolean;

	    static createFrom(source: any = {}) {
	        return new OrchestratorConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.autoTriage = source["autoTriage"];
	        this.autoPlan = source["autoPlan"];
	    }
	}
	export class ProviderEntryConfig {
	    enabled: boolean;
	    rateLimitCooldownSeconds: number;

	    static createFrom(source: any = {}) {
	        return new ProviderEntryConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.rateLimitCooldownSeconds = source["rateLimitCooldownSeconds"];
	    }
	}
	export class ProviderHealthCheckConfig {
	    enabled: boolean;
	    intervalSeconds: number;

	    static createFrom(source: any = {}) {
	        return new ProviderHealthCheckConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.intervalSeconds = source["intervalSeconds"];
	    }
	}
	export class ProvidersConfig {
	    healthCheck: ProviderHealthCheckConfig;
	    claude: ProviderEntryConfig;
	    codex: ProviderEntryConfig;
	    autoFailover: boolean;

	    static createFrom(source: any = {}) {
	        return new ProvidersConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.healthCheck = this.convertValues(source["healthCheck"], ProviderHealthCheckConfig);
	        this.claude = this.convertValues(source["claude"], ProviderEntryConfig);
	        this.codex = this.convertValues(source["codex"], ProviderEntryConfig);
	        this.autoFailover = source["autoFailover"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RenovateConfig {
	    enabled: boolean;
	    author: string;

	    static createFrom(source: any = {}) {
	        return new RenovateConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.author = source["author"];
	    }
	}
	export class TodoistConfig {
	    enabled: boolean;
	    apiToken: string;
	    projectId: string;
	    defaultProjectId: string;
	    pollSeconds: number;

	    static createFrom(source: any = {}) {
	        return new TodoistConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.apiToken = source["apiToken"];
	        this.projectId = source["projectId"];
	        this.defaultProjectId = source["defaultProjectId"];
	        this.pollSeconds = source["pollSeconds"];
	    }
	}

}

export namespace github {

	export class CheckRunInfo {
	    name: string;
	    status: string;
	    conclusion: string;

	    static createFrom(source: any = {}) {
	        return new CheckRunInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.status = source["status"];
	        this.conclusion = source["conclusion"];
	    }
	}
	export class Issue {
	    number: number;
	    title: string;
	    body: string;
	    url: string;
	    repository: string;
	    repoName: string;
	    labels: string[];
	    author: string;
	    createdAt: string;
	    updatedAt: string;

	    static createFrom(source: any = {}) {
	        return new Issue(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.number = source["number"];
	        this.title = source["title"];
	        this.body = source["body"];
	        this.url = source["url"];
	        this.repository = source["repository"];
	        this.repoName = source["repoName"];
	        this.labels = source["labels"];
	        this.author = source["author"];
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
	    }
	}
	export class PullRequest {
	    number: number;
	    title: string;
	    url: string;
	    repository: string;
	    repoName: string;
	    author: string;
	    isDraft: boolean;
	    labels: string[];
	    headRefName: string;
	    ciStatus: string;
	    reviewDecision: string;
	    mergeable: string;
	    unresolvedCount: number;
	    viewerHasApproved: boolean;
	    createdAt: string;
	    updatedAt: string;

	    static createFrom(source: any = {}) {
	        return new PullRequest(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.number = source["number"];
	        this.title = source["title"];
	        this.url = source["url"];
	        this.repository = source["repository"];
	        this.repoName = source["repoName"];
	        this.author = source["author"];
	        this.isDraft = source["isDraft"];
	        this.labels = source["labels"];
	        this.headRefName = source["headRefName"];
	        this.ciStatus = source["ciStatus"];
	        this.reviewDecision = source["reviewDecision"];
	        this.mergeable = source["mergeable"];
	        this.unresolvedCount = source["unresolvedCount"];
	        this.viewerHasApproved = source["viewerHasApproved"];
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
	    }
	}
	export class RenovatePR {
	    number: number;
	    title: string;
	    url: string;
	    repository: string;
	    repoName: string;
	    author: string;
	    isDraft: boolean;
	    labels: string[];
	    headRefName: string;
	    ciStatus: string;
	    reviewDecision: string;
	    mergeable: string;
	    unresolvedCount: number;
	    viewerHasApproved: boolean;
	    createdAt: string;
	    updatedAt: string;
	    checkRuns: CheckRunInfo[];

	    static createFrom(source: any = {}) {
	        return new RenovatePR(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.number = source["number"];
	        this.title = source["title"];
	        this.url = source["url"];
	        this.repository = source["repository"];
	        this.repoName = source["repoName"];
	        this.author = source["author"];
	        this.isDraft = source["isDraft"];
	        this.labels = source["labels"];
	        this.headRefName = source["headRefName"];
	        this.ciStatus = source["ciStatus"];
	        this.reviewDecision = source["reviewDecision"];
	        this.mergeable = source["mergeable"];
	        this.unresolvedCount = source["unresolvedCount"];
	        this.viewerHasApproved = source["viewerHasApproved"];
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
	        this.checkRuns = this.convertValues(source["checkRuns"], CheckRunInfo);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ReviewSummary {
	    createdByMe: PullRequest[];
	    reviewRequested: PullRequest[];

	    static createFrom(source: any = {}) {
	        return new ReviewSummary(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.createdByMe = this.convertValues(source["createdByMe"], PullRequest);
	        this.reviewRequested = this.convertValues(source["reviewRequested"], PullRequest);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace loopagent {

	export class LoopAgent {
	    id: string;
	    name: string;
	    prompt: string;
	    intervalSec: number;
	    allowedTools: string[];
	    provider: string;
	    model: string;
	    enabled: boolean;
	    // Go type: time
	    lastRunAt: any;
	    lastRunId: string;
	    lastRunCost: number;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;

	    static createFrom(source: any = {}) {
	        return new LoopAgent(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.prompt = source["prompt"];
	        this.intervalSec = source["intervalSec"];
	        this.allowedTools = source["allowedTools"];
	        this.provider = source["provider"];
	        this.model = source["model"];
	        this.enabled = source["enabled"];
	        this.lastRunAt = this.convertValues(source["lastRunAt"], null);
	        this.lastRunId = source["lastRunId"];
	        this.lastRunCost = source["lastRunCost"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace monitor {

	export class Anomaly {
	    kind: string;
	    taskId?: string;
	    severity: string;
	    requiresLlm: boolean;
	    fingerprint: string;
	    evidence?: Record<string, any>;
	    // Go type: time
	    detectedAt: any;

	    static createFrom(source: any = {}) {
	        return new Anomaly(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.taskId = source["taskId"];
	        this.severity = source["severity"];
	        this.requiresLlm = source["requiresLlm"];
	        this.fingerprint = source["fingerprint"];
	        this.evidence = source["evidence"];
	        this.detectedAt = this.convertValues(source["detectedAt"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Counts {
	    new: number;
	    todo: number;
	    inProgress: number;
	    inReview: number;
	    planReview: number;
	    humanRequired: number;
	    done: number;
	    byStatus: Record<string, number>;

	    static createFrom(source: any = {}) {
	        return new Counts(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.new = source["new"];
	        this.todo = source["todo"];
	        this.inProgress = source["inProgress"];
	        this.inReview = source["inReview"];
	        this.planReview = source["planReview"];
	        this.humanRequired = source["humanRequired"];
	        this.done = source["done"];
	        this.byStatus = source["byStatus"];
	    }
	}
	export class Report {
	    // Go type: time
	    generatedAt: any;
	    counts: Counts;
	    anomalies: Anomaly[];
	    remediated: string[];
	    dispatched: string[];
	    issuesOpened: number;
	    issuesUpdated: number;

	    static createFrom(source: any = {}) {
	        return new Report(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.generatedAt = this.convertValues(source["generatedAt"], null);
	        this.counts = this.convertValues(source["counts"], Counts);
	        this.anomalies = this.convertValues(source["anomalies"], Anomaly);
	        this.remediated = source["remediated"];
	        this.dispatched = source["dispatched"];
	        this.issuesOpened = source["issuesOpened"];
	        this.issuesUpdated = source["issuesUpdated"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace notification {

	export class Notification {
	    id: string;
	    level: string;
	    title: string;
	    message: string;
	    taskId?: string;
	    agentId?: string;
	    createdAt: string;

	    static createFrom(source: any = {}) {
	        return new Notification(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.level = source["level"];
	        this.title = source["title"];
	        this.message = source["message"];
	        this.taskId = source["taskId"];
	        this.agentId = source["agentId"];
	        this.createdAt = source["createdAt"];
	    }
	}

}

export namespace project {

	export class SandboxConfig {
	    image?: string;
	    build?: string;
	    with?: string[];
	    composeFile?: string;
	    service?: string;
	    port?: number;
	    env?: Record<string, string>;
	    envFile?: string;
	    cluster?: string;
	    deploy?: string;

	    static createFrom(source: any = {}) {
	        return new SandboxConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.image = source["image"];
	        this.build = source["build"];
	        this.with = source["with"];
	        this.composeFile = source["composeFile"];
	        this.service = source["service"];
	        this.port = source["port"];
	        this.env = source["env"];
	        this.envFile = source["envFile"];
	        this.cluster = source["cluster"];
	        this.deploy = source["deploy"];
	    }
	}
	export class Project {
	    id: string;
	    name: string;
	    owner: string;
	    repo: string;
	    url: string;
	    clonePath: string;
	    type: string;
	    setupCommands?: string[];
	    sandbox?: SandboxConfig;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;

	    static createFrom(source: any = {}) {
	        return new Project(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.owner = source["owner"];
	        this.repo = source["repo"];
	        this.url = source["url"];
	        this.clonePath = source["clonePath"];
	        this.type = source["type"];
	        this.setupCommands = source["setupCommands"];
	        this.sandbox = this.convertValues(source["sandbox"], SandboxConfig);
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

	export class Worktree {
	    path: string;
	    branch: string;
	    taskId: string;
	    head: string;

	    static createFrom(source: any = {}) {
	        return new Worktree(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.branch = source["branch"];
	        this.taskId = source["taskId"];
	        this.head = source["head"];
	    }
	}

}

export namespace provider {

	export class Status {
	    provider: string;
	    healthy: boolean;
	    reason: string;
	    detail?: string;
	    // Go type: time
	    lastCheck: any;
	    // Go type: time
	    ratelimitedUntil: any;

	    static createFrom(source: any = {}) {
	        return new Status(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.provider = source["provider"];
	        this.healthy = source["healthy"];
	        this.reason = source["reason"];
	        this.detail = source["detail"];
	        this.lastCheck = this.convertValues(source["lastCheck"], null);
	        this.ratelimitedUntil = this.convertValues(source["ratelimitedUntil"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace stats {

	export class Summary {
	    totalCostUsd: number;
	    totalRuns: number;
	    avgCostPerRun: number;
	    avgDurationS: number;
	    totalDurationS: number;
	    totalInputTokens: number;
	    totalOutputTokens: number;

	    static createFrom(source: any = {}) {
	        return new Summary(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.totalCostUsd = source["totalCostUsd"];
	        this.totalRuns = source["totalRuns"];
	        this.avgCostPerRun = source["avgCostPerRun"];
	        this.avgDurationS = source["avgDurationS"];
	        this.totalDurationS = source["totalDurationS"];
	        this.totalInputTokens = source["totalInputTokens"];
	        this.totalOutputTokens = source["totalOutputTokens"];
	    }
	}
	export class GroupedStat {
	    key: string;
	    stats: Summary;

	    static createFrom(source: any = {}) {
	        return new GroupedStat(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.stats = this.convertValues(source["stats"], Summary);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RunRecord {
	    id: string;
	    taskId: string;
	    projectId?: string;
	    mode: string;
	    role: string;
	    model?: string;
	    provider?: string;
	    costUsd: number;
	    durationS: number;
	    inputTokens?: number;
	    outputTokens?: number;
	    outcome: string;
	    // Go type: time
	    timestamp: any;

	    static createFrom(source: any = {}) {
	        return new RunRecord(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.taskId = source["taskId"];
	        this.projectId = source["projectId"];
	        this.mode = source["mode"];
	        this.role = source["role"];
	        this.model = source["model"];
	        this.provider = source["provider"];
	        this.costUsd = source["costUsd"];
	        this.durationS = source["durationS"];
	        this.inputTokens = source["inputTokens"];
	        this.outputTokens = source["outputTokens"];
	        this.outcome = source["outcome"];
	        this.timestamp = this.convertValues(source["timestamp"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class StatsResponse {
	    today: Summary;
	    thisWeek: Summary;
	    thisMonth: Summary;
	    allTime: Summary;
	    byProject: GroupedStat[];
	    byMode: GroupedStat[];
	    byRole: GroupedStat[];
	    byModel: GroupedStat[];
	    byProvider: GroupedStat[];
	    recentRuns: RunRecord[];

	    static createFrom(source: any = {}) {
	        return new StatsResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.today = this.convertValues(source["today"], Summary);
	        this.thisWeek = this.convertValues(source["thisWeek"], Summary);
	        this.thisMonth = this.convertValues(source["thisMonth"], Summary);
	        this.allTime = this.convertValues(source["allTime"], Summary);
	        this.byProject = this.convertValues(source["byProject"], GroupedStat);
	        this.byMode = this.convertValues(source["byMode"], GroupedStat);
	        this.byRole = this.convertValues(source["byRole"], GroupedStat);
	        this.byModel = this.convertValues(source["byModel"], GroupedStat);
	        this.byProvider = this.convertValues(source["byProvider"], GroupedStat);
	        this.recentRuns = this.convertValues(source["recentRuns"], RunRecord);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace synapse {

	export class LoggingSettings {
	    level: string;
	    maxSizeMB: number;
	    maxFiles: number;

	    static createFrom(source: any = {}) {
	        return new LoggingSettings(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.level = source["level"];
	        this.maxSizeMB = source["maxSizeMB"];
	        this.maxFiles = source["maxFiles"];
	    }
	}
	export class AppSettings {
	    agent: config.AgentDefaults;
	    notification: config.NotificationConfig;
	    orchestrator: config.OrchestratorConfig;
	    logging: LoggingSettings;
	    audit: config.AuditConfig;
	    todoist: config.TodoistConfig;
	    renovate: config.RenovateConfig;
	    providers: config.ProvidersConfig;
	    directories: Record<string, string>;

	    static createFrom(source: any = {}) {
	        return new AppSettings(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.agent = this.convertValues(source["agent"], config.AgentDefaults);
	        this.notification = this.convertValues(source["notification"], config.NotificationConfig);
	        this.orchestrator = this.convertValues(source["orchestrator"], config.OrchestratorConfig);
	        this.logging = this.convertValues(source["logging"], LoggingSettings);
	        this.audit = this.convertValues(source["audit"], config.AuditConfig);
	        this.todoist = this.convertValues(source["todoist"], config.TodoistConfig);
	        this.renovate = this.convertValues(source["renovate"], config.RenovateConfig);
	        this.providers = this.convertValues(source["providers"], config.ProvidersConfig);
	        this.directories = source["directories"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

	export class LoopAgentRun {
	    agentId: string;
	    // Go type: time
	    startedAt: any;
	    // Go type: time
	    finishedAt: any;
	    costUsd: number;
	    state: string;
	    durationS: number;

	    static createFrom(source: any = {}) {
	        return new LoopAgentRun(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.agentId = source["agentId"];
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.finishedAt = this.convertValues(source["finishedAt"], null);
	        this.costUsd = source["costUsd"];
	        this.state = source["state"];
	        this.durationS = source["durationS"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class MonitorReportBinding {
	    enabled: boolean;
	    ready: boolean;
	    report: monitor.Report;

	    static createFrom(source: any = {}) {
	        return new MonitorReportBinding(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.ready = source["ready"];
	        this.report = this.convertValues(source["report"], monitor.Report);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class VersionInfo {
	    server: string;

	    static createFrom(source: any = {}) {
	        return new VersionInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.server = source["server"];
	    }
	}

}

export namespace task {

	export class AgentRun {
	    agentId: string;
	    role: string;
	    mode: string;
	    provider?: string;
	    state: string;
	    // Go type: time
	    startedAt: any;
	    costUsd: number;
	    result: string;
	    logFile: string;
	    sessionId?: string;

	    static createFrom(source: any = {}) {
	        return new AgentRun(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.agentId = source["agentId"];
	        this.role = source["role"];
	        this.mode = source["mode"];
	        this.provider = source["provider"];
	        this.state = source["state"];
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.costUsd = source["costUsd"];
	        this.result = source["result"];
	        this.logFile = source["logFile"];
	        this.sessionId = source["sessionId"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ReviewComment {
	    id: string;
	    line: number;
	    body: string;
	    resolved: boolean;
	    // Go type: time
	    createdAt: any;

	    static createFrom(source: any = {}) {
	        return new ReviewComment(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.line = source["line"];
	        this.body = source["body"];
	        this.resolved = source["resolved"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Task {
	    id: string;
	    slug: string;
	    title: string;
	    status: string;
	    taskType: string;
	    agentMode: string;
	    allowedTools: string[];
	    tags: string[];
	    projectId: string;
	    branch: string;
	    prNumber: number;
	    issue: string;
	    statusReason: string;
	    reviewed: boolean;
	    runRole: string;
	    todoistId: string;
	    // Go type: time
	    dueDate?: any;
	    requirePermissions?: boolean;
	    agentRuns: AgentRun[];
	    workflow?: workflow.Execution;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	    body: string;
	    plan?: string;
	    planCritique?: string;
	    filePath: string;

	    static createFrom(source: any = {}) {
	        return new Task(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.slug = source["slug"];
	        this.title = source["title"];
	        this.status = source["status"];
	        this.taskType = source["taskType"];
	        this.agentMode = source["agentMode"];
	        this.allowedTools = source["allowedTools"];
	        this.tags = source["tags"];
	        this.projectId = source["projectId"];
	        this.branch = source["branch"];
	        this.prNumber = source["prNumber"];
	        this.issue = source["issue"];
	        this.statusReason = source["statusReason"];
	        this.reviewed = source["reviewed"];
	        this.runRole = source["runRole"];
	        this.todoistId = source["todoistId"];
	        this.dueDate = this.convertValues(source["dueDate"], null);
	        this.requirePermissions = source["requirePermissions"];
	        this.agentRuns = this.convertValues(source["agentRuns"], AgentRun);
	        this.workflow = this.convertValues(source["workflow"], workflow.Execution);
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	        this.body = source["body"];
	        this.plan = source["plan"];
	        this.planCritique = source["planCritique"];
	        this.filePath = source["filePath"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace todoist {

	export class Project {
	    id: string;
	    name: string;

	    static createFrom(source: any = {}) {
	        return new Project(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	    }
	}

}

export namespace workflow {

	export class Condition {
	    field: string;
	    operator: string;
	    value: string;

	    static createFrom(source: any = {}) {
	        return new Condition(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.field = source["field"];
	        this.operator = source["operator"];
	        this.value = source["value"];
	    }
	}
	export class Transition {
	    when?: Condition;
	    goto: string;

	    static createFrom(source: any = {}) {
	        return new Transition(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.when = this.convertValues(source["when"], Condition);
	        this.goto = source["goto"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class StepConfig {
	    role: string;
	    mode: string;
	    model: string;
	    provider: string;
	    prompt: string;
	    allowedTools: string[];
	    needsWorktree: boolean;
	    humanActions: string[];
	    status: string;
	    statusReason: string;
	    check?: Condition;
	    maxRetries: number;
	    reuseAgent: boolean;
	    waitForStatus: string;
	    command: string;
	    dir: string;

	    static createFrom(source: any = {}) {
	        return new StepConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.role = source["role"];
	        this.mode = source["mode"];
	        this.model = source["model"];
	        this.provider = source["provider"];
	        this.prompt = source["prompt"];
	        this.allowedTools = source["allowedTools"];
	        this.needsWorktree = source["needsWorktree"];
	        this.humanActions = source["humanActions"];
	        this.status = source["status"];
	        this.statusReason = source["statusReason"];
	        this.check = this.convertValues(source["check"], Condition);
	        this.maxRetries = source["maxRetries"];
	        this.reuseAgent = source["reuseAgent"];
	        this.waitForStatus = source["waitForStatus"];
	        this.command = source["command"];
	        this.dir = source["dir"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Step {
	    id: string;
	    name: string;
	    type: string;
	    config: StepConfig;
	    next: Transition[];
	    parallel: Step[];
	    position?: Position;

	    static createFrom(source: any = {}) {
	        return new Step(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.type = source["type"];
	        this.config = this.convertValues(source["config"], StepConfig);
	        this.next = this.convertValues(source["next"], Transition);
	        this.parallel = this.convertValues(source["parallel"], Step);
	        this.position = this.convertValues(source["position"], Position);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Position {
	    x: number;
	    y: number;

	    static createFrom(source: any = {}) {
	        return new Position(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.x = source["x"];
	        this.y = source["y"];
	    }
	}
	export class Trigger {
	    on: string;
	    priority: number;
	    conditions: Condition[];
	    position?: Position;

	    static createFrom(source: any = {}) {
	        return new Trigger(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.on = source["on"];
	        this.priority = source["priority"];
	        this.conditions = this.convertValues(source["conditions"], Condition);
	        this.position = this.convertValues(source["position"], Position);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Definition {
	    id: string;
	    name: string;
	    description: string;
	    trigger: Trigger;
	    steps: Step[];
	    builtin: boolean;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;

	    static createFrom(source: any = {}) {
	        return new Definition(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.description = source["description"];
	        this.trigger = this.convertValues(source["trigger"], Trigger);
	        this.steps = this.convertValues(source["steps"], Step);
	        this.builtin = source["builtin"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class StepRecord {
	    stepId: string;
	    status: string;
	    output: string;
	    agentId: string;
	    provider?: string;
	    // Go type: time
	    startedAt: any;
	    // Go type: time
	    endedAt: any;

	    static createFrom(source: any = {}) {
	        return new StepRecord(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.stepId = source["stepId"];
	        this.status = source["status"];
	        this.output = source["output"];
	        this.agentId = source["agentId"];
	        this.provider = source["provider"];
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.endedAt = this.convertValues(source["endedAt"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Execution {
	    workflowId: string;
	    currentStep: string;
	    state: string;
	    stepHistory: StepRecord[];
	    variables: Record<string, string>;
	    // Go type: time
	    startedAt: any;
	    // Go type: time
	    completedAt?: any;
	    recovered?: boolean;

	    static createFrom(source: any = {}) {
	        return new Execution(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.workflowId = source["workflowId"];
	        this.currentStep = source["currentStep"];
	        this.state = source["state"];
	        this.stepHistory = this.convertValues(source["stepHistory"], StepRecord);
	        this.variables = source["variables"];
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.completedAt = this.convertValues(source["completedAt"], null);
	        this.recovered = source["recovered"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}






}


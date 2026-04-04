export namespace agent {
	
	export class Agent {
	    id: string;
	    taskId: string;
	    mode: string;
	    state: string;
	    sessionId: string;
	    tmuxSession: string;
	    costUsd: number;
	    inputTokens?: number;
	    outputTokens?: number;
	    // Go type: time
	    startedAt: any;
	    external: boolean;
	    pid?: number;
	    command?: string;
	    name?: string;
	    project?: string;
	    model?: string;
	
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
	        this.tmuxSession = source["tmuxSession"];
	        this.costUsd = source["costUsd"];
	        this.inputTokens = source["inputTokens"];
	        this.outputTokens = source["outputTokens"];
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.external = source["external"];
	        this.pid = source["pid"];
	        this.command = source["command"];
	        this.name = source["name"];
	        this.project = source["project"];
	        this.model = source["model"];
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
	export class StreamEvent {
	    type: string;
	    content?: string;
	    session_id?: string;
	    cost_usd?: number;
	    input_tokens?: number;
	    output_tokens?: number;
	    subtype?: string;
	
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
	    }
	}

}

export namespace github {
	
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
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
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
	
	export class Project {
	    id: string;
	    name: string;
	    owner: string;
	    repo: string;
	    url: string;
	    clonePath: string;
	    type: string;
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

export namespace task {
	
	export class AgentRun {
	    agentId: string;
	    role: string;
	    mode: string;
	    state: string;
	    // Go type: time
	    startedAt: any;
	    costUsd: number;
	    result: string;
	    logFile: string;
	
	    static createFrom(source: any = {}) {
	        return new AgentRun(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.agentId = source["agentId"];
	        this.role = source["role"];
	        this.mode = source["mode"];
	        this.state = source["state"];
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.costUsd = source["costUsd"];
	        this.result = source["result"];
	        this.logFile = source["logFile"];
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
	    agentMode: string;
	    allowedTools: string[];
	    tags: string[];
	    projectId: string;
	    branch: string;
	    prNumber: number;
	    issue: string;
	    reviewed: boolean;
	    agentRuns: AgentRun[];
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	    body: string;
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
	        this.agentMode = source["agentMode"];
	        this.allowedTools = source["allowedTools"];
	        this.tags = source["tags"];
	        this.projectId = source["projectId"];
	        this.branch = source["branch"];
	        this.prNumber = source["prNumber"];
	        this.issue = source["issue"];
	        this.reviewed = source["reviewed"];
	        this.agentRuns = this.convertValues(source["agentRuns"], AgentRun);
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	        this.body = source["body"];
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

export namespace tmux {
	
	export class SessionInfo {
	    name: string;
	    created: string;
	
	    static createFrom(source: any = {}) {
	        return new SessionInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.created = source["created"];
	    }
	}

}


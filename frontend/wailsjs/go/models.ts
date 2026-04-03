export namespace agent {
	
	export class Agent {
	    id: string;
	    taskId: string;
	    mode: string;
	    state: string;
	    sessionId: string;
	    tmuxSession: string;
	    costUsd: number;
	    // Go type: time
	    startedAt: any;
	    external: boolean;
	    pid?: number;
	    command?: string;
	    name?: string;
	    project?: string;
	
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
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.external = source["external"];
	        this.pid = source["pid"];
	        this.command = source["command"];
	        this.name = source["name"];
	        this.project = source["project"];
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
	    ciStatus: string;
	    reviewDecision: string;
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
	        this.ciStatus = source["ciStatus"];
	        this.reviewDecision = source["reviewDecision"];
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

export namespace project {
	
	export class Project {
	    id: string;
	    name: string;
	    owner: string;
	    repo: string;
	    url: string;
	    clonePath: string;
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

export namespace task {
	
	export class AgentRun {
	    agentId: string;
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
	    title: string;
	    status: string;
	    agentMode: string;
	    allowedTools: string[];
	    tags: string[];
	    projectId: string;
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
	        this.title = source["title"];
	        this.status = source["status"];
	        this.agentMode = source["agentMode"];
	        this.allowedTools = source["allowedTools"];
	        this.tags = source["tags"];
	        this.projectId = source["projectId"];
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


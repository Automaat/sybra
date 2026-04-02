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
	
	    static createFrom(source: any = {}) {
	        return new StreamEvent(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.content = source["content"];
	        this.session_id = source["session_id"];
	        this.cost_usd = source["cost_usd"];
	    }
	}

}

export namespace task {
	
	export class Task {
	    id: string;
	    title: string;
	    status: string;
	    agentMode: string;
	    allowedTools: string[];
	    tags: string[];
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


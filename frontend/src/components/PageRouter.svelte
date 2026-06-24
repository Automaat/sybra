<script lang="ts">
  // Eager: the daily-driver pages reachable on first paint (default route is
  // task-list) — keeping them static avoids a navigation chunk fetch on the
  // hot path.
  import TaskList from '../pages/TaskList.svelte'
  import TaskDetail from '../pages/TaskDetail.svelte'
  import Dashboard from '../pages/Dashboard.svelte'
  import TaskSidebar from './TaskSidebar.svelte'
  import { navStore } from '../lib/navigation.svelte.js'
  import { lazyComponent } from '../lib/lazy.js'

  // Lazy: rarer / heavier routes, each emitted as its own async chunk so they
  // stay out of the initial bundle. WorkflowDetail alone pulls @xyflow
  // (~80 kB gzip); GitHub/Stats/Reviews/Settings add more page-only code.
  const loadAgents = lazyComponent(() => import('../pages/Agents.svelte'))
  const loadAgentDetail = lazyComponent(() => import('../pages/AgentDetail.svelte'))
  const loadProjectList = lazyComponent(() => import('../pages/ProjectList.svelte'))
  const loadProjectDetail = lazyComponent(() => import('../pages/ProjectDetail.svelte'))
  const loadGitHub = lazyComponent(() => import('../pages/GitHub.svelte'))
  const loadStats = lazyComponent(() => import('../pages/Stats.svelte'))
  const loadReviews = lazyComponent(() => import('../pages/Reviews.svelte'))
  const loadSettings = lazyComponent(() => import('../pages/Settings.svelte'))
  const loadChatList = lazyComponent(() => import('../pages/ChatList.svelte'))
  const loadChatDetail = lazyComponent(() => import('../pages/ChatDetail.svelte'))
  const loadWorkflowList = lazyComponent(() => import('../pages/WorkflowList.svelte'))
  const loadWorkflowDetail = lazyComponent(() => import('../pages/WorkflowDetail.svelte'))
  const loadLogbook = lazyComponent(() => import('../pages/Logbook.svelte'))

  interface Props {
    sidebarTaskId: string | null
    onsidebarclose: () => void
    onfocusedtaskchange: (id: string | null) => void
    navTaskDetail: (id: string) => void
    navAgentDetail: (id: string) => void
    navChatDetail: (id: string) => void
    navProjectDetail: (id: string) => void
    navWorkflowDetail: (id: string) => void
    onnewTask: () => void
    onnewProject: () => void
    onselectTaskFromList: (id: string) => void
  }

  const {
    sidebarTaskId,
    onsidebarclose,
    onfocusedtaskchange,
    navTaskDetail,
    navAgentDetail,
    navChatDetail,
    navProjectDetail,
    navWorkflowDetail,
    onnewTask,
    onnewProject,
    onselectTaskFromList,
  }: Props = $props()
</script>

<main class="flex min-h-0 flex-1 {navStore.page.kind === 'task-list' && sidebarTaskId ? 'flex-row overflow-hidden' : 'flex-col overflow-y-auto'}">
  {#if navStore.page.kind === 'dashboard'}
    <Dashboard onviewagent={navAgentDetail} />
  {:else if navStore.page.kind === 'task-list'}
    <div class="flex min-h-0 flex-1 flex-col overflow-hidden">
      <TaskList
        onselect={onselectTaskFromList}
        filter={navStore.page.filter}
        onnewTask={onnewTask}
        onfocusedtaskchange={onfocusedtaskchange}
      />
    </div>
    {#if sidebarTaskId}
      <TaskSidebar
        taskId={sidebarTaskId}
        onclose={onsidebarclose}
        onviewagent={navAgentDetail}
        onviewtask={(id) => { onsidebarclose(); navTaskDetail(id) }}
      />
    {/if}
  {:else if navStore.page.kind === 'task-detail'}
    <TaskDetail
      taskId={navStore.page.taskId}
      onback={() => navStore.back()}
      onviewagent={navAgentDetail}
      ondelete={() => navStore.back()}
      onreviewplan={() => navStore.reset({ kind: 'reviews' })}
    />
  {:else if navStore.page.kind === 'project-list'}
    {#await loadProjectList() then ProjectList}
      <ProjectList
        onselect={navProjectDetail}
        onadd={onnewProject}
      />
    {/await}
  {:else if navStore.page.kind === 'project-detail'}
    {#await loadProjectDetail() then ProjectDetail}
      <ProjectDetail
        projectId={navStore.page.projectId}
        onback={() => navStore.back()}
        onviewtask={navTaskDetail}
      />
    {/await}
  {:else if navStore.page.kind === 'chats'}
    {#await loadChatList() then ChatList}
      <ChatList onselect={navChatDetail} />
    {/await}
  {:else if navStore.page.kind === 'chat-detail'}
    {#await loadChatDetail() then ChatDetail}
      <ChatDetail
        agentId={navStore.page.agentId}
        onback={() => navStore.back()}
        onviewtask={navTaskDetail}
      />
    {/await}
  {:else if navStore.page.kind === 'agents'}
    {#await loadAgents() then Agents}
      <Agents
        initialTab={navStore.page.tab}
        onselect={navAgentDetail}
      />
    {/await}
  {:else if navStore.page.kind === 'agent-detail'}
    {#await loadAgentDetail() then AgentDetail}
      <AgentDetail
        agentId={navStore.page.agentId}
        onback={() => navStore.back()}
        onviewtask={navTaskDetail}
        onnavigate={navAgentDetail}
      />
    {/await}
  {:else if navStore.page.kind === 'github'}
    {#await loadGitHub() then GitHub}
      <GitHub />
    {/await}
  {:else if navStore.page.kind === 'reviews'}
    {#await loadReviews() then Reviews}
      <Reviews onviewtask={navTaskDetail} />
    {/await}
  {:else if navStore.page.kind === 'stats'}
    {#await loadStats() then Stats}
      <Stats />
    {/await}
  {:else if navStore.page.kind === 'workflows'}
    {#await loadWorkflowList() then WorkflowList}
      <WorkflowList onselect={navWorkflowDetail} />
    {/await}
  {:else if navStore.page.kind === 'workflow-detail'}
    {#await loadWorkflowDetail() then WorkflowDetail}
      <WorkflowDetail
        workflowId={navStore.page.workflowId}
        onback={() => navStore.back()}
      />
    {/await}
  {:else if navStore.page.kind === 'logbook'}
    {#await loadLogbook() then Logbook}
      <Logbook onviewtask={navTaskDetail} />
    {/await}
  {:else if navStore.page.kind === 'settings'}
    {#await loadSettings() then Settings}
      <Settings />
    {/await}
  {/if}
</main>

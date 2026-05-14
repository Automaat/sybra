<script lang="ts">
  import TaskList from '../pages/TaskList.svelte'
  import TaskDetail from '../pages/TaskDetail.svelte'
  import Agents from '../pages/Agents.svelte'
  import AgentDetail from '../pages/AgentDetail.svelte'
  import ProjectList from '../pages/ProjectList.svelte'
  import ProjectDetail from '../pages/ProjectDetail.svelte'
  import Dashboard from '../pages/Dashboard.svelte'
  import GitHub from '../pages/GitHub.svelte'
  import Stats from '../pages/Stats.svelte'
  import Reviews from '../pages/Reviews.svelte'
  import Settings from '../pages/Settings.svelte'
  import ChatList from '../pages/ChatList.svelte'
  import ChatDetail from '../pages/ChatDetail.svelte'
  import WorkflowList from '../pages/WorkflowList.svelte'
  import WorkflowDetail from '../pages/WorkflowDetail.svelte'
  import Logbook from '../pages/Logbook.svelte'
  import TaskSidebar from './TaskSidebar.svelte'
  import { navStore } from '../lib/navigation.svelte.js'

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
    <ProjectList
      onselect={navProjectDetail}
      onadd={onnewProject}
    />
  {:else if navStore.page.kind === 'project-detail'}
    <ProjectDetail
      projectId={navStore.page.projectId}
      onback={() => navStore.back()}
      onviewtask={navTaskDetail}
    />
  {:else if navStore.page.kind === 'chats'}
    <ChatList onselect={navChatDetail} />
  {:else if navStore.page.kind === 'chat-detail'}
    <ChatDetail
      agentId={navStore.page.agentId}
      onback={() => navStore.back()}
      onviewtask={navTaskDetail}
    />
  {:else if navStore.page.kind === 'agents'}
    <Agents
      initialTab={navStore.page.tab}
      onselect={navAgentDetail}
    />
  {:else if navStore.page.kind === 'agent-detail'}
    <AgentDetail
      agentId={navStore.page.agentId}
      onback={() => navStore.back()}
      onviewtask={navTaskDetail}
      onnavigate={navAgentDetail}
    />
  {:else if navStore.page.kind === 'github'}
    <GitHub />
  {:else if navStore.page.kind === 'reviews'}
    <Reviews onviewtask={navTaskDetail} />
  {:else if navStore.page.kind === 'stats'}
    <Stats />
  {:else if navStore.page.kind === 'workflows'}
    <WorkflowList onselect={navWorkflowDetail} />
  {:else if navStore.page.kind === 'workflow-detail'}
    <WorkflowDetail
      workflowId={navStore.page.workflowId}
      onback={() => navStore.back()}
    />
  {:else if navStore.page.kind === 'logbook'}
    <Logbook onviewtask={navTaskDetail} />
  {:else if navStore.page.kind === 'settings'}
    <Settings />
  {/if}
</main>

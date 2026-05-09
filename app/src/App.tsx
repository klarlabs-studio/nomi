import { useCallback, useEffect, useRef, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import { useQuery } from "@tanstack/react-query";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Badge } from "@/components/ui/badge";
import { ChatInterface } from "@/components/chat-interface";
import { AssistantManager } from "@/components/assistant-manager";
import { ApprovalPanel } from "@/components/approval-panel";
import { MemoryInspector } from "@/components/memory-inspector";
import { EventLog } from "@/components/event-log";
import { ProviderSettings } from "@/components/provider-settings";
import { PluginsManager } from "@/components/plugins-manager";
import { SafetySettings } from "@/components/safety-settings";
import { AboutSettings } from "@/components/about-settings";
import { UpdateBanner } from "@/components/update-banner";
import { OnboardingWizard } from "@/components/onboarding/wizard";
import { EventProvider } from "@/providers/event-provider";
import { approvalsApi, assistantsApi, healthApi, runsApi, settingsApi } from "@/lib/api";
import { queryKeys } from "@/lib/query-keys";
import type { Assistant } from "@/types/api";
import {
  MessageSquare,
  Bot,
  Shield,
  Brain,
  Radio,
  Sparkles,
  Puzzle,
  ShieldCheck,
  Info,
} from "lucide-react";

function ConnectionStatus() {
  // Health check via React Query so the cache is shared with anything
  // else that might want to render the daemon's reachability, and so
  // we don't run a parallel setInterval that competes with the Query
  // refetch loop. retry: false because the failure mode IS the signal.
  const { data, isLoading, isError } = useQuery({
    queryKey: queryKeys.health.check(),
    queryFn: () => healthApi.check(),
    refetchInterval: 5000,
    refetchIntervalInBackground: true,
    retry: false,
    staleTime: 0,
  });

  if (isLoading && !data && !isError) {
    return <Badge variant="outline">Checking...</Badge>;
  }

  const connected = !isError && !!data;
  return (
    <Badge variant={connected ? "default" : "destructive"}>
      {connected ? "Connected" : "Disconnected"}
    </Badge>
  );
}

type MainTab = "chats" | "assistants" | "approvals" | "memory" | "events" | "settings";
type SettingsTab = "plugins" | "ai-providers" | "safety" | "about";

// Ordered list of every sidebar entry. Order determines keyboard-arrow
// traversal and wrap-around. Kept as a flat array so the arrow handler can
// just do index arithmetic.
const SIDEBAR_TABS: {
  id: MainTab;
  label: string;
  icon: React.ElementType;
  section: "Chat" | "System" | "Settings";
  settingsSub?: SettingsTab;
}[] = [
  { id: "chats", label: "Chats", icon: MessageSquare, section: "Chat" },
  { id: "assistants", label: "Assistants", icon: Bot, section: "Chat" },
  { id: "approvals", label: "Approvals", icon: Shield, section: "Chat" },
  { id: "memory", label: "Memory", icon: Brain, section: "System" },
  { id: "events", label: "Events", icon: Radio, section: "System" },
  {
    id: "settings",
    label: "Plugins",
    icon: Puzzle,
    section: "Settings",
    settingsSub: "plugins",
  },
  {
    id: "settings",
    label: "AI Providers",
    icon: Sparkles,
    section: "Settings",
    settingsSub: "ai-providers",
  },
  {
    id: "settings",
    label: "Safety",
    icon: ShieldCheck,
    section: "Settings",
    settingsSub: "safety",
  },
  {
    id: "settings",
    label: "About",
    icon: Info,
    section: "Settings",
    settingsSub: "about",
  },
];

// Title + subtitle for the main header per tab. Keeping this beside the tab
// declarations makes it impossible to add a tab without remembering to label it.
const TAB_HEADERS: Record<MainTab, { title: string; subtitle: string }> = {
  chats: { title: "Chats", subtitle: "Your conversations with Nomi agents" },
  assistants: { title: "Assistants", subtitle: "Manage your AI assistants" },
  approvals: { title: "Approvals", subtitle: "Pending actions requiring your approval" },
  memory: { title: "Memory", subtitle: "Agent memories and context" },
  events: { title: "Events", subtitle: "System event log" },
  settings: { title: "Settings", subtitle: "Configure Nomi" },
};

function tabPanelID(mainTab: MainTab, settingsSub: SettingsTab): string {
  return mainTab === "settings" ? `panel-settings-${settingsSub}` : `panel-${mainTab}`;
}

function tabTriggerID(mainTab: MainTab, settingsSub?: SettingsTab): string {
  return mainTab === "settings" && settingsSub
    ? `tab-settings-${settingsSub}`
    : `tab-${mainTab}`;
}

function SidebarItem({
  icon: Icon,
  label,
  active,
  panelID,
  triggerID,
  tabIndex,
  onActivate,
  onKeyDown,
  badge,
  itemRef,
}: {
  icon: React.ElementType;
  label: string;
  active: boolean;
  panelID: string;
  triggerID: string;
  tabIndex: number;
  onActivate: () => void;
  onKeyDown: (e: React.KeyboardEvent<HTMLButtonElement>) => void;
  badge?: number;
  itemRef: (el: HTMLButtonElement | null) => void;
}) {
  return (
    <button
      type="button"
      ref={itemRef}
      role="tab"
      id={triggerID}
      aria-selected={active}
      aria-controls={panelID}
      tabIndex={tabIndex}
      onClick={onActivate}
      onKeyDown={onKeyDown}
      className={`w-full flex items-center gap-3 px-3 py-2 rounded-md text-sm transition-colors ${
        active
          ? "bg-primary/10 text-primary font-medium"
          : "text-muted-foreground hover:text-foreground hover:bg-muted"
      }`}
    >
      <Icon className="w-4 h-4" />
      <span className="flex-1 text-left">{label}</span>
      {badge !== undefined && badge > 0 && (
        <Badge variant="secondary" className="text-xs h-5 min-w-[20px] justify-center">
          {badge}
        </Badge>
      )}
    </button>
  );
}

function App() {
  const [mainTab, setMainTab] = useState<MainTab>("chats");
  const [settingsTab, setSettingsTab] = useState<SettingsTab>("plugins");
  const [chatResetToken, setChatResetToken] = useState(0);
  // Deep-link target set by ApprovalPanel when the user clicks "Open in
  // chat". ChatInterface reads it as a one-shot prop and copies it into
  // its own selectedChat state.
  const [deepLinkChatId, setDeepLinkChatId] = useState<string | null>(null);
  const [checkingOnboarding, setCheckingOnboarding] = useState(true);
  const [showOnboarding, setShowOnboarding] = useState(false);
  const [onboardingTemplates, setOnboardingTemplates] = useState<Assistant[]>([]);

  useEffect(() => {
    const loadOnboardingState = async () => {
      try {
        const [status, templates] = await Promise.all([
          settingsApi.getOnboardingComplete(),
          assistantsApi.listTemplates(),
        ]);
        setShowOnboarding(!status.complete);
        setOnboardingTemplates(templates.templates);
      } catch {
        setShowOnboarding(false);
      } finally {
        setCheckingOnboarding(false);
      }
    };
    void loadOnboardingState();
  }, []);

  useEffect(() => {
    let unlisten: UnlistenFn | null = null;

    const subscribe = async () => {
      try {
        unlisten = await listen<string>("tray-menu-clicked", async (event) => {
          const action = event.payload;
          if (action === "settings") {
            setMainTab("settings");
            return;
          }
          if (action === "new-chat") {
            setMainTab("chats");
            setChatResetToken((v) => v + 1);
            return;
          }
          if (action === "approvals") {
            setMainTab("approvals");
            return;
          }
          if (action === "pause-agents") {
            try {
              const { runs } = await runsApi.list();
              const active = runs.filter(
                (run) => run.status === "executing" || run.status === "awaiting_approval",
              );
              await Promise.all(active.map((run) => runsApi.pause(run.id).catch(() => null)));
            } catch {
              // Best effort only; individual chats still expose Pause.
            }
          }
        });
      } catch {
        // Running outside Tauri (e.g. vite preview / tests).
      }
    };

    void subscribe();
    return () => {
      if (unlisten) {
        void unlisten();
      }
    };
  }, []);

  // Tray badge + status icon. We piggy-back on React Query — EventProvider
  // already invalidates approvals.list and runs.list on every approval.* /
  // run.* event, so this effect re-fires within ~100ms of state changes.
  // The 30s refetch interval is a slow-path safety net for cases the SSE
  // bridge missed (vite preview, transient disconnect).
  const approvalsQuery = useQuery({
    queryKey: queryKeys.approvals.list(),
    queryFn: () => approvalsApi.list(),
    refetchInterval: 30_000,
  });
  const runsQuery = useQuery({
    queryKey: queryKeys.runs.list(),
    queryFn: () => runsApi.list(),
    refetchInterval: 30_000,
  });

  useEffect(() => {
    const pendingCount = (approvalsQuery.data?.approvals ?? []).filter(
      (a) => a.status === "pending",
    ).length;
    const hasActiveRun = (runsQuery.data?.runs ?? []).some(
      (r) => r.status === "executing",
    );
    // awaiting trumps active so the user always notices an approval first.
    const trayState =
      pendingCount > 0 ? "awaiting" : hasActiveRun ? "active" : "idle";

    void invoke("set_approvals_badge", { count: pendingCount }).catch(() => {});
    void invoke("set_tray_state", { state: trayState }).catch(() => {});
  }, [approvalsQuery.data, runsQuery.data]);

  // Refs to every tab button, in SIDEBAR_TABS order, so arrow keys can
  // focus the neighbor.
  const tabRefs = useRef<(HTMLButtonElement | null)[]>([]);

  // The currently-active tab index in the flattened SIDEBAR_TABS array.
  const activeIndex = SIDEBAR_TABS.findIndex((tab) =>
    tab.id === "settings"
      ? mainTab === "settings" && settingsTab === tab.settingsSub
      : mainTab === tab.id,
  );

  const activate = useCallback((i: number) => {
    const tab = SIDEBAR_TABS[i];
    if (!tab) return;
    setMainTab(tab.id);
    if (tab.id === "settings" && tab.settingsSub) {
      setSettingsTab(tab.settingsSub);
    }
    // Move keyboard focus to the activated tab so screen readers announce it
    // and the visible focus ring follows.
    window.requestAnimationFrame(() => tabRefs.current[i]?.focus());
  }, []);

  const handleKey = useCallback(
    (i: number) => (e: React.KeyboardEvent<HTMLButtonElement>) => {
      let next = i;
      switch (e.key) {
        case "ArrowDown":
        case "ArrowRight":
          next = (i + 1) % SIDEBAR_TABS.length;
          break;
        case "ArrowUp":
        case "ArrowLeft":
          next = (i - 1 + SIDEBAR_TABS.length) % SIDEBAR_TABS.length;
          break;
        case "Home":
          next = 0;
          break;
        case "End":
          next = SIDEBAR_TABS.length - 1;
          break;
        default:
          return;
      }
      e.preventDefault();
      activate(next);
    },
    [activate],
  );

  // Group tabs by section for the rendered sidebar.
  const sections: { name: "Chat" | "System" | "Settings"; tabs: number[] }[] = [
    { name: "Chat", tabs: [] },
    { name: "System", tabs: [] },
    { name: "Settings", tabs: [] },
  ];
  SIDEBAR_TABS.forEach((tab, idx) => {
    const section = sections.find((s) => s.name === tab.section);
    section?.tabs.push(idx);
  });

  const header = TAB_HEADERS[mainTab];
  const activePanelID = tabPanelID(mainTab, settingsTab);

  if (checkingOnboarding) {
    return (
      <EventProvider>
        <div className="min-h-screen bg-background text-foreground flex items-center justify-center">
          <Badge variant="outline">Loading...</Badge>
        </div>
      </EventProvider>
    );
  }

  if (showOnboarding) {
    return (
      <EventProvider>
        <OnboardingWizard
          templates={onboardingTemplates}
          onComplete={() => {
            setShowOnboarding(false);
            setMainTab("chats");
          }}
          onSkip={async () => {
            try {
              await settingsApi.setOnboardingComplete(true);
            } finally {
              setShowOnboarding(false);
            }
          }}
        />
      </EventProvider>
    );
  }

  return (
    <EventProvider>
    <div className="min-h-screen bg-background text-foreground flex flex-col">
      <UpdateBanner />
      <div className="flex-1 flex">
      {/* Left Sidebar — tablist for keyboard-driven navigation */}
      <div
        className="w-56 border-r flex flex-col"
        role="tablist"
        aria-orientation="vertical"
        aria-label="Nomi navigation"
      >
        {/* Header */}
        <div className="p-4 border-b">
          <div className="flex items-center gap-2">
            <div className="w-7 h-7 bg-primary rounded-md flex items-center justify-center">
              <span className="text-primary-foreground font-bold text-xs">N</span>
            </div>
            <span className="font-semibold text-lg">Nomi</span>
          </div>
        </div>

        {sections.map((section) => (
          <div key={section.name} className="p-2 space-y-1">
            <p className="px-3 py-1 text-xs font-medium text-muted-foreground uppercase tracking-wider">
              {section.name}
            </p>
            {section.tabs.map((idx) => {
              const tab = SIDEBAR_TABS[idx];
              const active = idx === activeIndex;
              return (
                <SidebarItem
                  key={idx}
                  icon={tab.icon}
                  label={tab.label}
                  active={active}
                  panelID={tabPanelID(tab.id, tab.settingsSub ?? settingsTab)}
                  triggerID={tabTriggerID(tab.id, tab.settingsSub)}
                  tabIndex={active ? 0 : -1}
                  onActivate={() => activate(idx)}
                  onKeyDown={handleKey(idx)}
                  itemRef={(el) => {
                    tabRefs.current[idx] = el;
                  }}
                />
              );
            })}
          </div>
        ))}

        <div className="mt-auto p-4 border-t">
          <ConnectionStatus />
        </div>
      </div>

      {/* Main Content — matching tabpanel */}
      <div
        className="flex-1 flex flex-col overflow-hidden"
        role="tabpanel"
        id={activePanelID}
        aria-labelledby={tabTriggerID(mainTab, settingsTab)}
      >
        <header className="border-b px-6 py-4">
          <div className="flex items-center justify-between">
            <div>
              <h1 className="text-xl font-semibold tracking-tight">{header.title}</h1>
              <p className="text-sm text-muted-foreground">{header.subtitle}</p>
            </div>
          </div>
        </header>

        <div className="flex-1 overflow-hidden">
          {mainTab === "chats" && (
            <ChatInterface resetToken={chatResetToken} deepLinkChatId={deepLinkChatId} />
          )}
          {mainTab === "assistants" && <AssistantManager />}
          {mainTab === "approvals" && (
            <ApprovalPanel
              onOpenChat={(runId) => {
                setDeepLinkChatId(runId);
                setMainTab("chats");
              }}
            />
          )}
          {mainTab === "memory" && <MemoryInspector />}
          {mainTab === "events" && <EventLog />}
          {mainTab === "settings" && (
            <Tabs
              value={settingsTab}
              onValueChange={(v) => setSettingsTab(v as SettingsTab)}
              className="h-full flex flex-col"
            >
              <div className="border-b px-6">
                <TabsList className="bg-transparent">
                  <TabsTrigger value="plugins">Plugins</TabsTrigger>
                  <TabsTrigger value="ai-providers">AI Providers</TabsTrigger>
                  <TabsTrigger value="safety">Safety</TabsTrigger>
                  <TabsTrigger value="about">About</TabsTrigger>
                </TabsList>
              </div>
              <TabsContent value="plugins" className="h-full m-0 overflow-auto">
                <PluginsManager />
              </TabsContent>
              <TabsContent value="ai-providers" className="h-full m-0 overflow-auto">
                <ProviderSettings />
              </TabsContent>
              <TabsContent value="safety" className="h-full m-0 overflow-auto">
                <SafetySettings />
              </TabsContent>
              <TabsContent value="about" className="h-full m-0 overflow-auto">
                <AboutSettings />
              </TabsContent>
            </Tabs>
          )}
        </div>
      </div>
      </div>
    </div>
    </EventProvider>
  );
}

export default App;

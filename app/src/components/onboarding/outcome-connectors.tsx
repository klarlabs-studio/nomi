import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { pluginsApi } from "@/lib/api";
import type { Plugin } from "@/types/api";
import {
  Mail,
  // lucide-react v1 dropped brand icons (Github/Slack/Discord/...). Use
  // GitBranch as a generic stand-in for the GitHub connector card; the
  // brand mark itself appears on the destination plugin page where the
  // plugin's own icon_url is rendered.
  GitBranch,
  MessageSquare,
  Calendar,
  BookOpen,
  Bot,
  Plug,
  ArrowRight,
  Check,
  Loader2,
} from "lucide-react";

export interface Outcome {
  id: string;
  title: string;
  description: string;
  pluginId: string;
  icon: React.ElementType;
  color: string;
}

// Outcome-to-plugin mapping. These plugin IDs must match the manifest IDs
// registered by the Go daemon. System plugins are always present; marketplace
// plugins may need installation first.
const OUTCOMES: Outcome[] = [
  {
    id: "read-email",
    title: "Read my email",
    description: "Connect Gmail so Nomi can read, summarize, and draft messages.",
    pluginId: "com.nomi.gmail",
    icon: Mail,
    color: "text-red-500",
  },
  {
    id: "github-prs",
    title: "Triage my GitHub PRs",
    description: "Review pull requests, check CI status, and leave comments.",
    pluginId: "com.nomi.github",
    icon: GitBranch,
    color: "text-slate-700 dark:text-slate-300",
  },
  {
    id: "slack-summary",
    title: "Summarize my Slack channels",
    description: "Catch up on unread messages and thread discussions.",
    pluginId: "com.nomi.slack",
    icon: MessageSquare,
    color: "text-purple-500",
  },
  {
    id: "telegram-chat",
    title: "Chat with my Telegram bot",
    description: "Talk to Nomi through Telegram messages.",
    pluginId: "com.nomi.telegram",
    icon: Bot,
    color: "text-blue-500",
  },
  {
    id: "calendar-manage",
    title: "Manage my calendar",
    description: "Check schedule, find free slots, and create events.",
    pluginId: "com.nomi.calendar",
    icon: Calendar,
    color: "text-emerald-500",
  },
  {
    id: "obsidian-notes",
    title: "Take notes from my Obsidian vault",
    description: "Read vault files and capture new ideas as notes.",
    pluginId: "com.nomi.obsidian",
    icon: BookOpen,
    color: "text-violet-500",
  },
];

type ConnectionState = "idle" | "connecting" | "connected" | "error";

interface OutcomeConnectorPickerProps {
  plugins: Plugin[];
  recommendedPluginIds?: string[];
  onOutcomeConnected?: (outcomeId: string, pluginId: string) => void;
  onSkip?: () => void;
  onDone?: () => void;
  mode?: "wizard" | "modal";
}

export function OutcomeConnectorPicker({
  plugins,
  recommendedPluginIds = [],
  onOutcomeConnected,
  onSkip,
  onDone,
  mode = "wizard",
}: OutcomeConnectorPickerProps) {
  const [connectingId, setConnectingId] = useState<string | null>(null);
  const [connectedIds, setConnectedIds] = useState<Set<string>>(new Set());
  const [errorIds, setErrorIds] = useState<Set<string>>(new Set());

  // Determine which outcomes are available (plugin is installed/enabled).
  const pluginMap = new Map(plugins.map((p) => [p.manifest.id, p]));

  const getOutcomeState = (outcome: Outcome): ConnectionState => {
    if (connectedIds.has(outcome.id)) return "connected";
    if (errorIds.has(outcome.id)) return "error";
    if (connectingId === outcome.id) return "connecting";
    return "idle";
  };

  const isPluginAvailable = (outcome: Outcome): boolean => {
    const plugin = pluginMap.get(outcome.pluginId);
    return !!plugin && plugin.state?.enabled !== false;
  };

  const isAlreadyConnected = (outcome: Outcome): boolean => {
    const plugin = pluginMap.get(outcome.pluginId);
    return !!plugin && plugin.connections.length > 0;
  };

  const handleConnect = async (outcome: Outcome) => {
    if (!isPluginAvailable(outcome)) return;

    setConnectingId(outcome.id);
    setErrorIds((prev) => {
      const next = new Set(prev);
      next.delete(outcome.id);
      return next;
    });

    try {
      const plugin = pluginMap.get(outcome.pluginId);
      if (!plugin) throw new Error("Plugin not found");

      // If the plugin already has a connection, treat it as done.
      if (plugin.connections.length > 0) {
        setConnectedIds((prev) => new Set(prev).add(outcome.id));
        onOutcomeConnected?.(outcome.id, outcome.pluginId);
        return;
      }

      // For plugins that require credentials (OAuth, tokens), we can't
      // fully automate the first connection from here — the user needs to
      // go through the plugin's auth flow. Instead, we create a placeholder
      // connection with a descriptive name and mark it as disabled, then
      // redirect the user to the plugin settings to complete setup.
      //
      // Future enhancement: for OAuth plugins, open the auth URL in a
      // browser and poll for completion.
      const schema = plugin.manifest.requires?.config_schema;
      const creds = plugin.manifest.requires?.credentials;

      if (!schema && !creds) {
        // No config needed — just create a default connection.
        await pluginsApi.createConnection(outcome.pluginId, {
          name: outcome.title,
          enabled: true,
        });
      } else {
        // Requires setup — create a skeleton connection so the user can
        // resume configuration in the Plugins tab.
        await pluginsApi.createConnection(outcome.pluginId, {
          name: outcome.title,
          enabled: false,
        });
      }

      setConnectedIds((prev) => new Set(prev).add(outcome.id));
      onOutcomeConnected?.(outcome.id, outcome.pluginId);
    } catch (err) {
      console.error("Failed to connect outcome:", err);
      setErrorIds((prev) => new Set(prev).add(outcome.id));
    } finally {
      setConnectingId(null);
    }
  };

  const availableOutcomes = OUTCOMES.filter(isPluginAvailable);
  const hasAnyAvailable = availableOutcomes.length > 0;

  return (
    <div className="space-y-4">
      <div className="space-y-1">
        <h2 className="font-medium text-lg">What do you want Nomi to do?</h2>
        <p className="text-sm text-muted-foreground">
          Connect services so Nomi can work with your existing tools. You can always add more later.
        </p>
      </div>

      {!hasAnyAvailable && (
        <div className="rounded-md border border-dashed p-6 text-center space-y-2">
          <Plug className="mx-auto h-8 w-8 text-muted-foreground" />
          <p className="text-sm text-muted-foreground">
            No connectors are installed yet. You can add them from the Plugins tab in Settings.
          </p>
        </div>
      )}

      <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
        {OUTCOMES.map((outcome) => {
          const state = getOutcomeState(outcome);
          const available = isPluginAvailable(outcome);
          const alreadyConnected = isAlreadyConnected(outcome);
          const Icon = outcome.icon;

          return (
            <button
              key={outcome.id}
              type="button"
              disabled={!available || state === "connecting"}
              onClick={() => handleConnect(outcome)}
              className={`text-left rounded-md border p-4 transition-colors hover:bg-muted/40 disabled:opacity-50 disabled:cursor-not-allowed ${
                state === "connected" || alreadyConnected
                  ? "border-emerald-300 bg-emerald-50 dark:border-emerald-700 dark:bg-emerald-950/30"
                  : state === "error"
                    ? "border-red-300 bg-red-50 dark:border-red-700 dark:bg-red-950/30"
                    : "border-border"
              }`}
            >
              <div className="flex items-start gap-3">
                <div className={`mt-0.5 ${outcome.color}`}>
                  <Icon className="h-5 w-5" />
                </div>
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-2 flex-wrap">
                    <p className="font-medium text-sm">{outcome.title}</p>
                    {recommendedPluginIds.includes(outcome.pluginId) && (
                      <Badge variant="outline" className="text-amber-600 border-amber-300 text-[10px]">
                        Recommended
                      </Badge>
                    )}
                    {(state === "connected" || alreadyConnected) && (
                      <Badge variant="outline" className="text-emerald-600 border-emerald-300">
                        <Check className="h-3 w-3 mr-0.5" />
                        Connected
                      </Badge>
                    )}
                    {!available && (
                      <Badge variant="secondary" className="text-xs">
                        Not installed
                      </Badge>
                    )}
                  </div>
                  <p className="text-xs text-muted-foreground mt-1">{outcome.description}</p>

                  {state === "connecting" && (
                    <div className="flex items-center gap-2 mt-2 text-xs text-muted-foreground">
                      <Loader2 className="h-3 w-3 animate-spin" />
                      Connecting…
                    </div>
                  )}

                  {state === "error" && (
                    <p className="text-xs text-red-600 mt-2">
                      Couldn&apos;t connect. Try again or set up manually in Plugins.
                    </p>
                  )}

                  {available && state !== "connected" && !alreadyConnected && (
                    <div className="flex items-center gap-1 mt-2 text-xs font-medium text-primary">
                      Connect
                      <ArrowRight className="h-3 w-3" />
                    </div>
                  )}
                </div>
              </div>
            </button>
          );
        })}
      </div>

      <div className="flex items-center justify-between pt-2">
        {mode === "wizard" && onSkip && (
          <Button type="button" variant="ghost" onClick={onSkip}>
            Skip for now
          </Button>
        )}
        {mode === "modal" && onSkip && (
          <Button type="button" variant="ghost" onClick={onSkip}>
            Close
          </Button>
        )}
        {onDone && (
          <Button
            type="button"
            onClick={onDone}
            className={mode === "wizard" ? "" : "w-full"}
          >
            {connectedIds.size > 0 ? "Continue" : "Done"}
          </Button>
        )}
      </div>
    </div>
  );
}

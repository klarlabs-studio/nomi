import { useEffect, useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { ToggleSwitch } from "@/components/ui/toggle-switch";
import { settingsApi } from "@/lib/api";
import { notificationsEnabled, setNotificationsEnabled } from "@/lib/notifications";
import type { SafetyProfile } from "@/types/api";

const DEFAULT_PROFILE: SafetyProfile = "balanced";

const PROFILE_COPY: Record<
  SafetyProfile,
  { title: string; summary: string; details: string[]; recommended?: boolean }
> = {
  cautious: {
    title: "Cautious",
    summary: "Ask before every sensitive action.",
    details: [
      "All capabilities require confirmation by default.",
      "Best for high-sensitivity workspaces.",
    ],
  },
  balanced: {
    title: "Balanced",
    summary: "Read freely, confirm writes and commands.",
    details: [
      "filesystem.read is allowed by default.",
      "filesystem.write, command.exec, and network calls need confirmation.",
    ],
    recommended: true,
  },
  fast: {
    title: "Fast",
    summary: "Allow most actions for rapid iteration.",
    details: [
      "Read/write/command are allowed by default.",
      "Network actions still require confirmation.",
    ],
  },
};

export function SafetySettings() {
  const [profile, setProfile] = useState<SafetyProfile>(DEFAULT_PROFILE);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const load = async () => {
      try {
        const data = await settingsApi.getSafetyProfile();
        setProfile(data.profile);
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err));
      } finally {
        setLoading(false);
      }
    };
    void load();
  }, []);

  const updateProfile = async (next: SafetyProfile) => {
    setProfile(next);
    setError(null);
    try {
      await settingsApi.setSafetyProfile(next);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div className="h-full overflow-auto p-6 space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>Safety Profile</CardTitle>
          <p className="text-sm text-muted-foreground">
            This default applies to newly created assistants. Existing assistants keep their current policies.
          </p>
        </CardHeader>
        <CardContent className="space-y-3">
          {loading ? (
            <p className="text-sm text-muted-foreground">Loading safety profile...</p>
          ) : (
            (Object.keys(PROFILE_COPY) as SafetyProfile[]).map((key) => {
              const data = PROFILE_COPY[key];
              const selected = profile === key;
              return (
                <label
                  key={key}
                  className={`block rounded-md border p-3 cursor-pointer ${
                    selected ? "border-primary bg-primary/5" : "border-border"
                  }`}
                >
                  <div className="flex items-start gap-2">
                    <input
                      type="radio"
                      name="safety-profile"
                      checked={selected}
                      onChange={() => void updateProfile(key)}
                      className="mt-1"
                    />
                    <div>
                      <p className="text-sm font-medium flex items-center gap-2">
                        {data.title}
                        {data.recommended && (
                          <span className="text-[10px] uppercase tracking-wide bg-primary/10 text-primary px-1.5 py-0.5 rounded">
                            Recommended
                          </span>
                        )}
                      </p>
                      <p className="text-xs text-muted-foreground">{data.summary}</p>
                      <ul className="text-xs mt-2 text-muted-foreground space-y-1">
                        {data.details.map((line) => (
                          <li key={line}>- {line}</li>
                        ))}
                      </ul>
                    </div>
                  </div>
                </label>
              );
            })
          )}
          {error && <p className="text-sm text-destructive">{error}</p>}
        </CardContent>
      </Card>

      <NotificationsSection />
    </div>
  );
}

function NotificationsSection() {
  const [enabled, setEnabled] = useState<boolean>(() => notificationsEnabled());
  const toggle = (next: boolean) => {
    setEnabled(next);
    setNotificationsEnabled(next);
  };
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Approval notifications</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        <p className="text-sm text-muted-foreground">
          When an assistant pauses for approval, Nomi fires an OS notification so you can
          respond without keeping the window focused. Permission is requested once on the
          first approval.
        </p>
        <label className="flex items-center justify-between gap-3 text-sm">
          <span>OS notifications when approval is needed</span>
          <ToggleSwitch checked={enabled} onChange={toggle} />
        </label>
      </CardContent>
    </Card>
  );
}

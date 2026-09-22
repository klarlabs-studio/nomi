/**
 * Marketplace browser (lifecycle-13).
 *
 * Lists every plugin in the NomiHub catalog as a card with capabilities,
 * size, publisher, and an Install button that hands off to the install
 * dialog with the entry as preset (so the trust panel pre-renders
 * with the catalog's own capability claims).
 *
 * Falls back to a friendly "marketplace not configured" panel when the
 * daemon returns 503 (NOMI_MARKETPLACE_ROOT_KEY not set). Falls back to
 * a "no entries" panel when the catalog is reachable but empty.
 *
 * Search box is local-only — filters in-memory across name/id/author/
 * description. Catalogs are small (target: <200 entries), no need for
 * server-side filtering.
 */
import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { pluginsApi, settingsApi, ApiError } from "@/lib/api";
import { errorMessage } from "@/lib/utils";
import type { MarketplaceEntry } from "@/types/api";
import { InstallPluginDialog } from "@/components/install-plugin-dialog";
import { Download, Package, RefreshCw, Search, ShieldCheck } from "lucide-react";

type Props = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
};

function MarketplaceCatalogURLPanel() {
  const qc = useQueryClient();
  const settingsQuery = useQuery({
    queryKey: ["marketplace-catalog-settings"],
    queryFn: () => settingsApi.getMarketplaceCatalog(),
  });
  const [urlDraft, setUrlDraft] = useState("");
  const [dirty, setDirty] = useState(false);
  const [localError, setLocalError] = useState<string | null>(null);

  const syncedURL = settingsQuery.data?.url ?? "";
  const displayURL = dirty ? urlDraft : syncedURL;
  const configured = settingsQuery.data?.configured ?? false;
  const defaultURL = settingsQuery.data?.default_url ?? "";
  const count = settingsQuery.data?.entry_count ?? 0;
  const lastError = settingsQuery.data?.last_error || localError;

  const save = useMutation({
    mutationFn: (url: string) => settingsApi.setMarketplaceCatalog(url),
    onSuccess: () => {
      setDirty(false);
      setLocalError(null);
      void qc.invalidateQueries({ queryKey: ["marketplace-catalog-settings"] });
      void qc.invalidateQueries({ queryKey: ["plugins", "marketplace"] });
    },
    onError: (err) => setLocalError(errorMessage(err)),
  });

  const refresh = useMutation({
    mutationFn: () => pluginsApi.refreshMarketplace(),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["marketplace-catalog-settings"] });
      void qc.invalidateQueries({ queryKey: ["plugins", "marketplace"] });
    },
    onError: (err) => setLocalError(errorMessage(err)),
  });

  if (!configured) {
    return (
      <div className="text-xs border rounded-md p-2.5 space-y-1 bg-muted/20">
        <p className="font-medium">Catalog URL</p>
        <p className="text-muted-foreground">
          Set <code className="font-mono">NOMI_MARKETPLACE_ROOT_KEY</code> to enable
          signed catalog browse. See{" "}
          <code className="font-mono">examples/wasm-marketplace-catalog/</code>.
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-2 rounded-md border border-dashed p-2.5">
      <div className="flex items-center justify-between gap-2">
        <p className="text-xs font-medium">Catalog URL</p>
        {count > 0 && (
          <Badge variant="secondary" className="text-[10px]">
            {count} plugins
          </Badge>
        )}
      </div>
      <p className="text-[11px] text-muted-foreground">
        Point at any signed NomiHub <code className="font-mono">index.json</code>{" "}
        (private mirror or local example catalog).
      </p>
      <div className="flex flex-wrap gap-1.5">
        <Input
          value={displayURL}
          onChange={(e) => {
            setUrlDraft(e.target.value);
            setDirty(true);
          }}
          placeholder={defaultURL || "https://…/index.json"}
          className="h-7 flex-1 min-w-[200px] text-xs font-mono"
        />
        <Button
          type="button"
          size="sm"
          className="h-7"
          disabled={save.isPending || (!dirty && displayURL === syncedURL)}
          onClick={() => save.mutate(displayURL.trim())}
        >
          Save
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          className="h-7"
          disabled={refresh.isPending}
          onClick={() => refresh.mutate()}
        >
          <RefreshCw className={`w-3 h-3 mr-1 ${refresh.isPending ? "animate-spin" : ""}`} />
          Refresh
        </Button>
      </div>
      <div className="flex flex-wrap gap-2 text-[11px]">
        {defaultURL && (
          <button
            type="button"
            className="underline text-muted-foreground hover:text-foreground"
            onClick={() => {
              setUrlDraft(defaultURL);
              setDirty(true);
            }}
          >
            Use default hub
          </button>
        )}
        {syncedURL && (
          <button
            type="button"
            className="underline text-muted-foreground hover:text-foreground"
            onClick={() => save.mutate("")}
          >
            Reset to default
          </button>
        )}
      </div>
      {lastError && <p className="text-[11px] text-destructive">{lastError}</p>}
    </div>
  );
}

export function MarketplaceBrowserDialog({ open, onOpenChange }: Props) {
  const [filter, setFilter] = useState("");
  const [installPreset, setInstallPreset] = useState<MarketplaceEntry | null>(null);

  const catalog = useQuery({
    queryKey: ["plugins", "marketplace"],
    queryFn: pluginsApi.marketplace,
    enabled: open,
    retry: false,
  });

  const filtered = useMemo(() => {
    const entries = catalog.data?.entries ?? [];
    if (!filter.trim()) return entries;
    const needle = filter.trim().toLowerCase();
    return entries.filter((e) =>
      [e.name, e.plugin_id, e.author ?? "", e.description ?? ""]
        .join(" ")
        .toLowerCase()
        .includes(needle),
    );
  }, [catalog.data, filter]);

  const isUnconfigured =
    catalog.error instanceof ApiError && catalog.error.status === 503;

  return (
    <>
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent className="max-w-3xl max-h-[80vh] flex flex-col">
          <DialogHeader>
            <DialogTitle>Browse marketplace</DialogTitle>
            <DialogDescription>
              Plugins published to NomiHub. Click Install to download, verify, and
              register a bundle into your runtime.
            </DialogDescription>
          </DialogHeader>

          <MarketplaceCatalogURLPanel />

          <div className="relative">
            <Search className="w-4 h-4 absolute left-2 top-1/2 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="Search by name, id, or author"
              className="pl-8"
              disabled={!catalog.data}
            />
          </div>

          <div className="flex-1 overflow-y-auto pr-1">
            {catalog.isLoading && (
              <div className="text-sm text-muted-foreground py-8 text-center">
                Loading catalog…
              </div>
            )}
            {isUnconfigured && (
              <div className="text-sm border rounded-md p-4 space-y-2">
                <div className="font-medium">Marketplace not configured</div>
                <p className="text-muted-foreground">
                  The marketplace needs the NomiHub root public key to verify
                  catalog signatures. Set <code>NOMI_MARKETPLACE_ROOT_KEY</code>{" "}
                  (base64-encoded ed25519 pubkey) before launching Nomi. Bundled
                  plugins continue to work without it. Generate a local catalog
                  with{" "}
                  <code className="font-mono">
                    go run ./examples/wasm-marketplace-catalog/gen
                  </code>
                  .
                </p>
              </div>
            )}
            {catalog.error && !isUnconfigured && (
              <div className="text-sm text-destructive border border-destructive rounded-md p-3">
                {errorMessage(catalog.error)}
              </div>
            )}
            {catalog.data && filtered.length === 0 && (
              <div className="text-sm text-muted-foreground py-8 text-center space-y-2">
                <p>
                  {filter
                    ? `No plugins match "${filter}"`
                    : "No plugins in the catalog yet."}
                </p>
                {!filter && (
                  <p className="text-xs">
                    Point the catalog URL at a signed index (see{" "}
                    <code className="font-mono">examples/wasm-marketplace-catalog/</code>
                    ) or publish with <code className="font-mono">nomi-publish catalog</code>.
                  </p>
                )}
              </div>
            )}
            {filtered.length > 0 && (
              <div className="space-y-2">
                {filtered.map((entry) => (
                  <CatalogEntryCard
                    key={entry.plugin_id}
                    entry={entry}
                    onInstall={() => {
                      setInstallPreset(entry);
                    }}
                  />
                ))}
              </div>
            )}
          </div>
        </DialogContent>
      </Dialog>

      {installPreset && (
        <InstallPluginDialog
          open={true}
          onOpenChange={(o) => {
            if (!o) setInstallPreset(null);
          }}
          preset={installPreset}
        />
      )}
    </>
  );
}

function CatalogEntryCard({
  entry,
  onInstall,
}: {
  entry: MarketplaceEntry;
  onInstall: () => void;
}) {
  return (
    <div className="border rounded-md p-3 space-y-2">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2 flex-wrap">
            <Package className="w-4 h-4 text-muted-foreground" />
            <span className="font-medium">{entry.name}</span>
            <Badge variant="outline" className="text-[10px]">
              v{entry.latest_version}
            </Badge>
            <span className="text-xs text-muted-foreground">
              {humanSize(entry.install_size_bytes)}
            </span>
          </div>
          <div className="text-xs text-muted-foreground mt-0.5">
            <code>{entry.plugin_id}</code>
            {entry.author && ` · ${entry.author}`}
          </div>
          {entry.description && (
            <p className="text-xs mt-1">{entry.description}</p>
          )}
          {entry.readme_excerpt && !entry.description && (
            <p className="text-xs mt-1 text-muted-foreground italic">
              {entry.readme_excerpt}
            </p>
          )}
          <div className="flex flex-wrap gap-1 mt-2">
            {entry.capabilities.slice(0, 4).map((c) => (
              <Badge
                key={c}
                variant="secondary"
                className="text-[10px] font-mono"
              >
                {c}
              </Badge>
            ))}
            {entry.capabilities.length > 4 && (
              <Badge variant="secondary" className="text-[10px]">
                +{entry.capabilities.length - 4}
              </Badge>
            )}
          </div>
          <div className="flex items-center gap-1 text-[10px] text-muted-foreground mt-2">
            <ShieldCheck className="w-3 h-3" />
            Signed by <code>{entry.publisher_fingerprint}</code>
          </div>
        </div>
        <div className="flex-shrink-0">
          <Button size="sm" onClick={onInstall}>
            <Download className="w-4 h-4 mr-1" /> Install
          </Button>
        </div>
      </div>
    </div>
  );
}

function humanSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
}

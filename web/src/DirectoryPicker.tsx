import { useEffect, useId, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  ArrowUp,
  ChevronRight,
  Folder,
  FolderOpen,
  FolderPlus,
  Home,
  RefreshCw,
} from "lucide-react";
import { api } from "./api";
import { Dialog } from "./components/ui/dialog";
import { Button } from "./components/ui/button";
type DirectoryPage = {
  path: string;
  parent: string;
  entries: { name: string; path: string }[];
  hasMore: boolean;
  nextAfter: string;
};
export function DirectoryPicker({
  open,
  onOpenChange,
  onSelect,
}: {
  open: boolean;
  onOpenChange: (value: boolean) => void;
  onSelect: (path: string) => void;
}) {
  const [path, setPath] = useState("");
  const [after, setAfter] = useState("");
  const [address, setAddress] = useState("");
  const [creation, setCreation] = useState<{
    name: string;
    error: string;
    pending: boolean;
  } | null>(null);
  const createRequest = useRef<object | null>(null);
  const newFolderButton = useRef<HTMLButtonElement>(null);
  const folderNameId = useId();
  const pending = creation?.pending ?? false;
  useEffect(() => {
    if (!open) setCreation(null);
    return () => {
      createRequest.current = null;
    };
  }, [open]);
  const listing = useQuery({
    queryKey: ["directories", path, after],
    queryFn: () =>
      api<DirectoryPage>("directories", { path, afterName: after }),
    enabled: open,
    gcTime: 0,
    retry: false,
  });
  function clearCreation() {
    createRequest.current = null;
    setCreation(null);
  }
  function changeOpen(next: boolean) {
    if (!next) clearCreation();
    onOpenChange(next);
  }
  function navigate(next: string, nextAfter = "") {
    clearCreation();
    setPath(next);
    setAfter(nextAfter);
    setAddress(next);
  }
  async function createFolder() {
    if (
      !creation ||
      !creation.name.trim() ||
      createRequest.current ||
      !listing.data ||
      listing.isFetching ||
      listing.isError
    )
      return;
    const request = {};
    createRequest.current = request;
    setCreation({ ...creation, pending: true, error: "" });
    try {
      const created = await api<{ path: string }>("directories/create", {
        path: listing.data.path,
        name: creation.name,
      });
      if (createRequest.current !== request) return;
      navigate(created.path);
    } catch (error) {
      if (createRequest.current !== request) return;
      createRequest.current = null;
      setCreation((current) =>
        current
          ? { ...current, pending: false, error: (error as Error).message }
          : null,
      );
    }
  }
  return (
    <Dialog
      open={open}
      onOpenChange={changeOpen}
      title="Choose a workspace folder"
      description="Browse directories on the machine running Meshra."
      wide
    >
      <div className="directory-toolbar">
        <Button
          type="button"
          variant="ghost"
          size="icon"
          aria-label="Home directory"
          disabled={pending}
          onClick={() => navigate("")}
        >
          <Home size={18} />
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          aria-label="Parent directory"
          disabled={
            pending ||
            !listing.data ||
            listing.data.path === listing.data.parent
          }
          onClick={() => navigate(listing.data!.parent)}
        >
          <ArrowUp size={18} />
        </Button>
        <input
          aria-label="Directory path"
          disabled={pending}
          value={address}
          placeholder={listing.data?.path || "Enter a directory path…"}
          onChange={(e) => setAddress(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              navigate(address);
            }
          }}
        />
        <Button
          type="button"
          variant="outline"
          disabled={pending}
          onClick={() => navigate(address)}
        >
          Go
        </Button>
      </div>
      <div className="directory-location">
        <FolderOpen size={16} />
        <span>{listing.data?.path || path || "Home"}</span>
      </div>
      <div className="directory-create">
        <Button
          ref={newFolderButton}
          type="button"
          variant="outline"
          aria-expanded={creation !== null}
          aria-controls={creation ? `${folderNameId}-entry` : undefined}
          disabled={
            pending || !listing.data || listing.isFetching || listing.isError
          }
          onClick={() => {
            if (creation) clearCreation();
            else setCreation({ name: "", error: "", pending: false });
          }}
        >
          <FolderPlus size={16} />
          New folder
        </Button>
        {creation && (
          <div
            id={`${folderNameId}-entry`}
            className="directory-create-entry"
            aria-busy={pending}
          >
            <label htmlFor={folderNameId}>Folder name</label>
            <div className="directory-create-actions">
              <input
                id={folderNameId}
                autoFocus
                autoComplete="off"
                value={creation.name}
                disabled={pending}
                aria-invalid={!!creation.error}
                aria-describedby={
                  creation.error ? `${folderNameId}-error` : undefined
                }
                onChange={(event) => {
                  const name = event.target.value;
                  setCreation((current) =>
                    current ? { ...current, name, error: "" } : null,
                  );
                }}
                onKeyDown={(event) => {
                  if (event.key === "Enter") {
                    event.preventDefault();
                    event.stopPropagation();
                    if (!event.nativeEvent.isComposing) void createFolder();
                  }
                }}
              />
              <Button
                type="button"
                disabled={
                  pending ||
                  !creation.name.trim() ||
                  !listing.data ||
                  listing.isFetching ||
                  listing.isError
                }
                onClick={() => void createFolder()}
              >
                {pending ? "Creating…" : "Create"}
              </Button>
              <Button
                type="button"
                variant="outline"
                disabled={pending}
                onClick={() => {
                  clearCreation();
                  newFolderButton.current?.focus();
                }}
              >
                Cancel
              </Button>
            </div>
            {pending && <p role="status">Creating folder…</p>}
            {creation.error && (
              <p id={`${folderNameId}-error`} role="alert">
                {creation.error}
              </p>
            )}
          </div>
        )}
      </div>
      <div
        className="directory-list"
        aria-label="Workspace folders"
        aria-busy={listing.isFetching}
      >
        {listing.isPending ? (
          <p role="status">Opening folders…</p>
        ) : listing.isError ? (
          <div role="alert">
            <p>{listing.error.message}</p>
            <Button
              type="button"
              variant="outline"
              disabled={pending}
              onClick={() => listing.refetch()}
            >
              <RefreshCw size={14} />
              Try again
            </Button>
          </div>
        ) : (
          <>
            {listing.data.entries.map((entry) => (
              <button
                key={entry.path}
                type="button"
                disabled={pending}
                onClick={() => navigate(entry.path)}
              >
                <Folder size={19} />
                <span>{entry.name}</span>
                <ChevronRight size={16} />
              </button>
            ))}
            {!listing.data.entries.length && (
              <p>No subfolders. You can select this directory.</p>
            )}
          </>
        )}
      </div>
      <div className="directory-paging">
        {after && (
          <Button
            type="button"
            variant="ghost"
            disabled={pending}
            onClick={() => navigate(listing.data?.path || path)}
          >
            First folders
          </Button>
        )}
        {listing.data?.hasMore && (
          <Button
            type="button"
            variant="ghost"
            disabled={pending}
            onClick={() =>
              navigate(listing.data!.path, listing.data!.nextAfter)
            }
          >
            More folders
            <ChevronRight size={14} />
          </Button>
        )}
      </div>
      <div className="dialog-footer">
        <Button
          type="button"
          variant="outline"
          onClick={() => changeOpen(false)}
        >
          Cancel
        </Button>
        <Button
          type="button"
          disabled={
            pending || !listing.data || listing.isFetching || listing.isError
          }
          onClick={() => {
            onSelect(listing.data!.path);
            changeOpen(false);
          }}
        >
          <FolderOpen size={16} />
          Use this folder
        </Button>
      </div>
    </Dialog>
  );
}

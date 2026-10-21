import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  ArrowUp,
  ChevronRight,
  Folder,
  FolderOpen,
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
  const listing = useQuery({
    queryKey: ["directories", path, after],
    queryFn: () =>
      api<DirectoryPage>("directories", { path, afterName: after }),
    enabled: open,
    gcTime: 0,
    retry: false,
  });
  function navigate(next: string) {
    setPath(next);
    setAfter("");
    setAddress(next);
  }
  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title="Choose a workspace folder"
      description="Browse directories on the machine running Meshra."
      wide
    >
      <div className="directory-toolbar">
        <Button
          variant="ghost"
          size="icon"
          aria-label="Home directory"
          onClick={() => navigate("")}
        >
          <Home size={18} />
        </Button>
        <Button
          variant="ghost"
          size="icon"
          aria-label="Parent directory"
          disabled={!listing.data || listing.data.path === listing.data.parent}
          onClick={() => navigate(listing.data!.parent)}
        >
          <ArrowUp size={18} />
        </Button>
        <input
          aria-label="Directory path"
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
        <Button variant="outline" onClick={() => navigate(address)}>
          Go
        </Button>
      </div>
      <div className="directory-location">
        <FolderOpen size={16} />
        <span>{listing.data?.path || path || "Home"}</span>
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
            <Button variant="outline" onClick={() => listing.refetch()}>
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
          <Button variant="ghost" onClick={() => setAfter("")}>
            First folders
          </Button>
        )}
        {listing.data?.hasMore && (
          <Button
            variant="ghost"
            onClick={() => setAfter(listing.data!.nextAfter)}
          >
            More folders
            <ChevronRight size={14} />
          </Button>
        )}
      </div>
      <div className="dialog-footer">
        <Button variant="outline" onClick={() => onOpenChange(false)}>
          Cancel
        </Button>
        <Button
          disabled={!listing.data || listing.isFetching || listing.isError}
          onClick={() => {
            onSelect(listing.data!.path);
            onOpenChange(false);
          }}
        >
          <FolderOpen size={16} />
          Use this folder
        </Button>
      </div>
    </Dialog>
  );
}

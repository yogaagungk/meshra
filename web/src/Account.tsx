import { useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./api";
import { Button } from "./components/ui/button";

type Account = { configured: boolean; username?: string };

export function AccountForm() {
  const client = useQueryClient();
  const account = useQuery({
    queryKey: ["account"],
    queryFn: () => api<Account>("account"),
    gcTime: 0,
  });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  async function save(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = e.currentTarget;
    const data = new FormData(form);
    setError("");
    setSaved(false);
    if (data.get("password") !== data.get("confirmation")) {
      setError(
        "The passwords do not match. Enter the same password in both fields.",
      );
      return;
    }
    setBusy(true);
    try {
      await api("account/save", {
        username: data.get("username"),
        password: data.get("password"),
        currentPassword: data.get("currentPassword") || "",
      });
      form.reset();
      await client.invalidateQueries({ queryKey: ["account"] });
      await client.invalidateQueries({ queryKey: ["auth"] });
      await client.invalidateQueries({ queryKey: ["sessions"] });
      setSaved(true);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <section>
      <h2>Username and password</h2>
      <p>
        {account.data?.configured
          ? "Update your sign-in details. Saving signs out your other browser sessions."
          : "Create sign-in details for your Meshra workspace. You can keep your administrator token for account recovery."}
      </p>
      {account.isPending && <p role="status">Loading account…</p>}
      {account.isError && <p role="alert">{account.error.message}</p>}
      {account.data && (
        <form
          className="personal-form"
          onSubmit={save}
          key={account.data.username || "setup"}
        >
          <label>
            Username
            <input
              name="username"
              autoComplete="username"
              autoCapitalize="none"
              spellCheck={false}
              defaultValue={account.data.username || ""}
              pattern={"[A-Za-z0-9][A-Za-z0-9_.\\-]{2,63}"}
              minLength={3}
              maxLength={64}
              required
              aria-describedby="username-help"
            />
          </label>
          <small id="username-help">
            3–64 letters, numbers, dots, underscores or hyphens. Start with a
            letter or number.
          </small>
          {account.data.configured && (
            <label>
              Current password or recovery token
              <input
                name="currentPassword"
                type="password"
                autoComplete="current-password"
                maxLength={1024}
                required
              />
            </label>
          )}
          <label>
            New password
            <input
              name="password"
              type="password"
              autoComplete="new-password"
              minLength={15}
              maxLength={1024}
              required
              aria-describedby="password-help"
            />
          </label>
          <small id="password-help">
            Use at least 15 characters. Spaces and passphrases are welcome.
          </small>
          <label>
            Confirm new password
            <input
              name="confirmation"
              type="password"
              autoComplete="new-password"
              minLength={15}
              maxLength={1024}
              required
            />
          </label>
          {error && <p role="alert">{error}</p>}
          {saved && (
            <p role="status">
              Saved. You can now sign in with your username and password.
            </p>
          )}
          <Button type="submit" disabled={busy}>
            {busy
              ? "Saving…"
              : account.data.configured
                ? "Update sign-in details"
                : "Enable username and password"}
          </Button>
        </form>
      )}
    </section>
  );
}

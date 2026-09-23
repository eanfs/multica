"use client";

import { Button } from "@multica/ui/components/ui/button";
import { useWorkspaceList } from "@multica/core/workspace";
import { useNavigation } from "@multica/views/navigation";
import { useT } from "@multica/views/i18n";
import { useLogout } from "@multica/views/auth";
import { auroraRoutes } from "@/lib/routes";

/**
 * The two ways out of a screen the user should not be on: open the workspace
 * they do have, or sign out.
 *
 * Shared by the two dead ends this app can show — a slug that names no
 * workspace, and a URL that names no route at all — because both leave the
 * user in exactly the same position, and a recovery that worked on one screen
 * and not the other would be a bug waiting to be found by half its users.
 */
export function WorkspaceRecovery() {
  const { t } = useT("aurora");
  const { t: tLayout } = useT("layout");
  const { replace } = useNavigation();
  const logout = useLogout();
  const { workspaces } = useWorkspaceList();
  const first = workspaces[0];

  return (
    <div className="flex flex-col gap-2 sm:flex-row">
      {/* Absent, not disabled, when the list holds nothing to open: the account
          has no destination, and the only way forward is signing in again,
          which the second button already offers. */}
      {first ? (
        <Button
          type="button"
          onClick={() => replace(auroraRoutes(first.slug).skills())}
        >
          {t(($) => $.workspace.open_workspace)}
        </Button>
      ) : null}
      <Button type="button" variant="outline" onClick={logout}>
        {tLayout(($) => $.sidebar.log_out)}
      </Button>
    </div>
  );
}

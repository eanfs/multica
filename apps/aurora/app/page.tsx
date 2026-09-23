import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { paths } from "@multica/core/paths";
import { auroraRoutes, isWorkspaceSlug } from "@/lib/routes";

/**
 * The app root. There is no landing page: a visitor either has a session and a
 * remembered workspace, or they sign in.
 *
 * Both facts come from cookies, which is why this is a server redirect rather
 * than a client effect — the decision needs no API call and no JavaScript, so
 * it can be made before anything renders. `multica_logged_in` carries no
 * authority (the session cookie does) and is only worth reading together with a
 * remembered slug; on its own it would send a signed-out visitor to a workspace
 * URL that bounces back to /login.
 */
export default async function RootPage() {
  const cookieStore = await cookies();
  const slug = cookieStore.get("multica_logged_in")
    ? cookieStore.get("last_workspace_slug")?.value
    : undefined;

  redirect(isWorkspaceSlug(slug) ? auroraRoutes(slug).skills() : paths.login());
}

import { SkillDirectory } from "@multica/views/aurora";
import { auroraRoutes } from "@/lib/routes";

/**
 * The skill directory.
 *
 * `topUpHref` is withheld: buying credits is Plan 5's checkout route and does
 * not exist yet. The shared views take the destination as a prop precisely so
 * that an app which has not built it offers nothing to click, rather than a
 * link that 404s.
 */
export default async function SkillsPage({
  params,
}: {
  params: Promise<{ workspaceSlug: string }>;
}) {
  const { workspaceSlug } = await params;
  return <SkillDirectory worksHref={auroraRoutes(workspaceSlug).works()} />;
}

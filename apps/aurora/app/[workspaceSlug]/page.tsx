import { redirect } from "next/navigation";
import { auroraRoutes } from "@/lib/routes";

/** A bare workspace URL opens the skill directory. */
export default async function WorkspaceIndexPage({
  params,
}: {
  params: Promise<{ workspaceSlug: string }>;
}) {
  const { workspaceSlug } = await params;
  redirect(auroraRoutes(workspaceSlug).skills());
}

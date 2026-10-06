import { RuntimeStatus } from "@multica/views/aurora";

/**
 * The workspace's managed execution node and its in-flight generations.
 *
 * Lifecycle actions are deliberately absent: creating, stopping and deleting a
 * node live in the runtime management surface, so this screen only reports the
 * node's state and points at the way back.
 */
export default function RuntimesPage() {
  return <RuntimeStatus />;
}

"use client";

import { Suspense, useEffect, useState } from "react";
import { useSearchParams, useRouter } from "next/navigation";
import { useQueryClient } from "@tanstack/react-query";
import { sanitizeNextUrl, useAuthStore } from "@multica/core/auth";
import { workspaceListOptions } from "@multica/core/workspace/queries";
import { createLogger } from "@multica/core/logger";
import { paths } from "@multica/core/paths";
import type { Workspace } from "@multica/core/types";
import {
  Card,
  CardHeader,
  CardTitle,
  CardDescription,
  CardContent,
} from "@multica/ui/components/ui/card";
import { useT } from "@multica/views/i18n";
import { Loader2 } from "lucide-react";
import { resolveAuroraDestination } from "@/lib/routes";
import { NoWorkspaceNotice } from "@/components/no-workspace-notice";
import { callbackErrorFrom, type CallbackError } from "./callback-error";

const authLogger = createLogger("aurora.auth.callback");

function CallbackContent() {
  const { t } = useT("auth");
  const router = useRouter();
  const searchParams = useSearchParams();
  const qc = useQueryClient();
  const loginWithGoogle = useAuthStore((s) => s.loginWithGoogle);
  const [error, setError] = useState<CallbackError | null>(null);
  const [noWorkspace, setNoWorkspace] = useState(false);

  useEffect(() => {
    const code = searchParams.get("code");
    const errorParam = searchParams.get("error");
    if (errorParam) {
      authLogger.warn("Google OAuth returned an error parameter", errorParam);
      setError(
        errorParam === "access_denied"
          ? { kind: "access_denied" }
          : { kind: "login_failed" },
      );
      return;
    }

    if (!code) {
      setError({ kind: "missing_code" });
      return;
    }

    // `state` round-trips through Google, so it is attacker-controlled by the
    // time it returns: anything salvaged from it is sanitized before use.
    //
    // Known debt, inherited from apps/web's identical flow and deliberately not
    // diverged here: this `state` is a carrier, not a CSRF nonce. It is whatever
    // the login page put in `state` and is never compared against a value the
    // browser kept, so nothing ties the code being exchanged to the browser that
    // started the flow — the shape of a login-CSRF
    // where a victim's browser is walked through someone else's authorization
    // code. The `sanitizeNextUrl` below bounds where the result can be sent, not
    // who it belongs to.
    //
    // The fix is to issue the nonce when the flow starts and require it back
    // before exchanging the code, which touches all three clients plus the
    // backend that owns the callback contract — a change for its own issue, not
    // for the app wiring that added this second copy of the flow.
    const stateParts = (searchParams.get("state") ?? "").split(",");
    const nextPart = stateParts.find((p) => p.startsWith("next:"));
    const nextUrl = sanitizeNextUrl(nextPart ? nextPart.slice(5) : null);

    loginWithGoogle(code, `${window.location.origin}/auth/callback`)
      .then(async () => {
        if (nextUrl) {
          router.push(nextUrl);
          return;
        }
        // The user may have signed in on this page rather than arriving from a
        // workspace route, so the list is fetched on demand. A failure here is
        // not an auth failure: it leaves the user without a destination, which
        // the notice below reports.
        const list = await qc
          .ensureQueryData(workspaceListOptions())
          .catch(() => [] as Workspace[]);
        const destination = resolveAuroraDestination(list);
        if (destination) router.push(destination);
        else setNoWorkspace(true);
      })
      .catch((err) => {
        authLogger.error("Google OAuth callback failed", err);
        setError(callbackErrorFrom(err));
      });
  }, [searchParams, loginWithGoogle, router, qc]);

  if (noWorkspace) return <NoWorkspaceNotice />;

  if (error) {
    const description = (() => {
      switch (error.kind) {
        case "raw":
          return error.text;
        case "missing_code":
          return t(($) => $.web.callback.missing_code);
        case "access_denied":
          return t(($) => $.web.callback.access_denied);
        case "login_failed":
          return t(($) => $.web.callback.login_failed);
        case "account_disabled":
          return t(($) => $.web.callback.account_disabled);
        case "signup_prohibited":
          return t(($) => $.web.callback.signup_prohibited);
        case "email_not_allowed":
          return t(($) => $.web.callback.email_not_allowed);
        case "google_account_no_email":
          return t(($) => $.web.callback.google_account_no_email);
        case "oauth_code_invalid":
          return t(($) => $.web.callback.oauth_code_invalid);
      }
    })();

    return (
      <div className="flex min-h-svh items-center justify-center px-6">
        <Card className="w-full max-w-sm">
          <CardHeader className="text-center">
            <CardTitle className="text-display-sm">
              {t(($) => $.web.callback.failed_title)}
            </CardTitle>
            <CardDescription>{description}</CardDescription>
          </CardHeader>
          <CardContent className="flex justify-center">
            <a
              href={paths.login()}
              className="text-primary underline-offset-4 hover:underline"
            >
              {t(($) => $.web.callback.back_to_login)}
            </a>
          </CardContent>
        </Card>
      </div>
    );
  }

  return (
    <div className="flex min-h-svh items-center justify-center px-6">
      <Card className="w-full max-w-sm">
        <CardHeader className="text-center">
          <CardTitle className="text-display-sm">
            {t(($) => $.web.callback.signing_in)}
          </CardTitle>
        </CardHeader>
        <CardContent className="flex justify-center">
          <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
        </CardContent>
      </Card>
    </div>
  );
}

export default function CallbackPage() {
  return (
    <Suspense fallback={null}>
      <CallbackContent />
    </Suspense>
  );
}

// @vitest-environment node

import { describe, expect, it } from "vitest";
import { ApiError } from "@multica/core/api";
import { callbackErrorFrom } from "./callback-error";

describe("callbackErrorFrom", () => {
  it.each([
    "account_disabled",
    "signup_prohibited",
    "email_not_allowed",
    "google_account_no_email",
    "oauth_code_invalid",
  ] as const)("keeps the stable %s error kind for localization", (code) => {
    const err = new ApiError("English fallback", 403, "Forbidden", { code });
    expect(callbackErrorFrom(err)).toEqual({ kind: code });
  });

  it("keeps an actionable message from an older server that returned an uncoded 4xx", () => {
    const err = new ApiError("registration is disabled", 403, "Forbidden");
    expect(callbackErrorFrom(err)).toEqual({
      kind: "raw",
      text: "registration is disabled",
    });
  });

  it("preserves unknown actionable codes as their server message", () => {
    const err = new ApiError("new signup restriction", 403, "Forbidden", {
      code: "future_restriction",
    });
    expect(callbackErrorFrom(err)).toEqual({
      kind: "raw",
      text: "new signup restriction",
    });
  });

  it("localizes a known 4xx code even without a fallback message", () => {
    const err = new ApiError("", 400, "Bad Request", {
      code: "oauth_code_invalid",
    });
    expect(callbackErrorFrom(err)).toEqual({ kind: "oauth_code_invalid" });
  });

  it("does not diagnose a 5xx from inside the server", () => {
    const err = new ApiError("boom", 500, "Internal Server Error", {
      code: "account_disabled",
    });
    expect(callbackErrorFrom(err)).toEqual({ kind: "login_failed" });
  });

  it("falls back to a generic failure for anything that is not an ApiError", () => {
    expect(callbackErrorFrom(new Error("network down"))).toEqual({
      kind: "login_failed",
    });
  });
});

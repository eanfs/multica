import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type {
  AuroraGeneration,
  AuroraSkill,
  AuroraTransaction,
} from "@multica/core/aurora";
import { renderWithI18n } from "../test/i18n";
import { stubNavigationAdapter } from "../test/navigation";
import { NavigationProvider } from "../navigation";

const mocks = vi.hoisted(() => ({
  balance: vi.fn(),
  transactions: vi.fn(),
  generations: vi.fn(),
  skills: vi.fn(),
}));

vi.mock("@multica/core/aurora", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/aurora")>();
  return {
    ...actual,
    useAuroraBalance: () => mocks.balance(),
    useAuroraTransactions: () => mocks.transactions(),
    useAuroraGenerations: () => mocks.generations(),
    useAuroraSkills: () => mocks.skills(),
  };
});

import { AuroraBilling } from "./billing";

const MICRO = 1_000_000;

const POSTER: AuroraSkill = {
  id: "poster",
  name: "海报制作",
  nameEn: "Poster",
  category: "image",
  credits: 760,
  input: ["text"],
  output: ["image"],
  featured: false,
  available: true,
};

const GENERATION: AuroraGeneration = {
  id: "gen-1",
  skillId: "poster",
  prompt: "a launch poster",
  status: "completed",
  creditsReserved: 760 * MICRO,
};

function transaction(
  overrides: Partial<AuroraTransaction> = {},
): AuroraTransaction {
  return {
    id: "txn-1",
    kind: "deduction",
    amountMicro: -760 * MICRO,
    balanceAfterMicro: 240 * MICRO,
    reference: "gen-1",
    createdAt: "2026-09-23T00:00:00Z",
    ...overrides,
  };
}

function renderBilling(props: Parameters<typeof AuroraBilling>[0] = {}) {
  return renderWithI18n(
    <NavigationProvider value={stubNavigationAdapter()}>
      <AuroraBilling {...props} />
    </NavigationProvider>,
  );
}

describe("AuroraBilling", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.balance.mockReturnValue({
      data: { availableMicro: 1000 * MICRO },
      isPending: false,
      // The error state's retry re-runs both reads, so the wallet needs one too
      // even in the cases that never reach it.
      refetch: vi.fn(),
    });
    mocks.transactions.mockReturnValue({
      data: [transaction()],
      isPending: false,
    });
    mocks.generations.mockReturnValue({ data: [GENERATION], isPending: false });
    mocks.skills.mockReturnValue({ data: [POSTER], isPending: false });
  });

  it("shows the wallet in credits rather than micro-credits", () => {
    renderBilling();

    expect(screen.getByText("1,000")).toBeInTheDocument();
    expect(screen.getByText("Available credits")).toBeInTheDocument();
  });

  it("attributes a generation's charge to the skill that made it", () => {
    renderBilling();

    // The ledger carries a generation id, which means nothing to a reader; the
    // catalog is what turns it back into a skill name.
    expect(screen.getByText("Poster")).toBeInTheDocument();
    expect(screen.getByText("−760")).toBeInTheDocument();
    expect(screen.getByText("Balance after: 240")).toBeInTheDocument();
  });

  it("falls back to the kind's label when the reference is not a generation", () => {
    // Grants and, later, subscription purchases key their rows with their own
    // references (`signup:`, `sub:`), which no catalog can resolve.
    mocks.transactions.mockReturnValue({
      data: [
        transaction({
          id: "txn-2",
          kind: "topup",
          amountMicro: 500 * MICRO,
          reference: "signup:2026-09",
        }),
      ],
      isPending: false,
    });

    renderBilling();

    expect(screen.getByText("Top-up")).toBeInTheDocument();
    expect(screen.getByText("+500")).toBeInTheDocument();
  });

  it("labels a ledger kind this build has never seen", () => {
    mocks.transactions.mockReturnValue({
      data: [
        transaction({ kind: "chargeback", reference: "unknown:1" }),
      ],
      isPending: false,
    });

    renderBilling();

    expect(screen.getByText("Other")).toBeInTheDocument();
  });

  it("shows the top-up entry as pending until the app supplies a route", () => {
    renderBilling();

    expect(screen.getByRole("button", { name: "Top up" })).toBeDisabled();
    expect(
      screen.getByText("Buying credits arrives with subscriptions."),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "Top up" }),
    ).not.toBeInTheDocument();
  });

  it("links the top-up entry once the app supplies a route", () => {
    renderBilling({ topUpHref: "/acme/billing/checkout" });

    expect(screen.getByRole("link", { name: "Top up" })).toHaveAttribute(
      "href",
      "/acme/billing/checkout",
    );
  });

  it("offers the empty copy before the wallet has moved", () => {
    mocks.transactions.mockReturnValue({ data: [], isPending: false });

    renderBilling();

    expect(screen.getByText("No activity yet")).toBeInTheDocument();
  });

  it("reports a failed load rather than an empty ledger", async () => {
    // "No activity yet" would be a lie with a way forward missing: a ledger
    // that would not load is not a ledger that has never moved.
    const user = userEvent.setup();
    const refetch = vi.fn();
    mocks.transactions.mockReturnValue({
      data: undefined,
      isPending: false,
      isError: true,
      refetch,
    });

    renderBilling();

    expect(
      screen.getByText("Could not load your credits"),
    ).toBeInTheDocument();
    expect(screen.queryByText("No activity yet")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(refetch).toHaveBeenCalled();
  });

  it("keeps the ledger it already read when a refetch fails", () => {
    // The same rule the library follows: a failed read is only fatal when it
    // left nothing behind. Replacing a readable balance and ledger with an
    // error card because one background refetch dropped is the worse trade.
    mocks.transactions.mockReturnValue({
      data: [transaction()],
      isPending: false,
      isError: true,
      refetch: vi.fn(),
    });

    renderBilling();

    expect(screen.getByText("Usage")).toBeInTheDocument();
    expect(
      screen.queryByText("Could not load your credits"),
    ).not.toBeInTheDocument();
  });
});

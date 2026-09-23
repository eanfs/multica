import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "@multica/core/api";
import type { AuroraGenerationDetail, AuroraSkill } from "@multica/core/aurora";
import { renderWithI18n } from "../test/i18n";
import { stubNavigationAdapter } from "../test/navigation";
import { NavigationProvider } from "../navigation";

// The composer's own job is the submit lifecycle: what it sends, what it shows
// while the server works, and which server refusal maps to which prompt. The
// transport is Task 1's module and is already covered there, so only its two
// hooks are replaced — `isAuroraInsufficientCreditsError` and its 429 sibling
// stay real, because classifying the error *is* the behaviour under test here.

const mocks = vi.hoisted(() => ({
  create: vi.fn(),
  detail: vi.fn(),
  detailRefetch: vi.fn(),
  balance: vi.fn(),
}));

vi.mock("@multica/core/aurora", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/aurora")>();
  return {
    ...actual,
    useCreateAuroraGeneration: () => ({
      mutateAsync: mocks.create,
      isPending: false,
    }),
    useAuroraGenerationDetail: (id: string) => mocks.detail(id),
    useAuroraBalance: () => mocks.balance(),
  };
});

import { GenerationComposer } from "./generation-composer";

const MICRO = 1_000_000;

function skill(overrides: Partial<AuroraSkill> = {}): AuroraSkill {
  return {
    id: "poster",
    name: "海报制作",
    nameEn: "Poster",
    category: "image",
    credits: 760,
    input: ["text", "image"],
    output: ["image"],
    featured: true,
    available: true,
    ...overrides,
  };
}

function detail(
  overrides: Partial<AuroraGenerationDetail> = {},
): AuroraGenerationDetail {
  return {
    id: "gen-1",
    skillId: "poster",
    prompt: "a launch poster",
    status: "queued",
    creditsReserved: 760 * MICRO,
    assets: [],
    ...overrides,
  };
}

function renderComposer(
  props: Partial<Parameters<typeof GenerationComposer>[0]>,
) {
  return renderWithI18n(
    <NavigationProvider value={stubNavigationAdapter()}>
      <GenerationComposer
        skill={skill()}
        open
        onOpenChange={vi.fn()}
        {...props}
      />
    </NavigationProvider>,
  );
}

async function submitPrompt(prompt = "a launch poster") {
  const user = userEvent.setup();
  await user.type(screen.getByLabelText("What should it make?"), prompt);
  await user.click(screen.getByRole("button", { name: "Generate" }));
  return user;
}

describe("GenerationComposer", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.detail.mockReturnValue({
      data: undefined,
      isPending: true,
      isError: false,
      refetch: mocks.detailRefetch,
    });
    mocks.balance.mockReturnValue({
      data: { availableMicro: 100 * MICRO },
      isPending: false,
    });
  });

  it("sends the prompt and follows the generation it started", async () => {
    mocks.create.mockResolvedValue(detail({ status: "queued" }));
    mocks.detail.mockReturnValue({
      data: detail({ status: "running" }),
      isPending: false,
    });

    renderComposer({});
    await submitPrompt();

    expect(mocks.create).toHaveBeenCalledWith({
      skillId: "poster",
      prompt: "a launch poster",
    });
    // The detail read is what the poll is attached to, so it must be asking
    // about the id the server just returned.
    expect(mocks.detail).toHaveBeenCalledWith("gen-1");
    expect(await screen.findByText("Generating")).toBeInTheDocument();
  });

  it("refuses to submit a skill that has not launched", () => {
    renderComposer({ skill: skill({ available: false }) });

    expect(screen.getByRole("button", { name: "Generate" })).toBeDisabled();
    expect(screen.getByText("Not available yet")).toBeInTheDocument();
  });

  it("refuses to submit an empty prompt", async () => {
    const user = userEvent.setup();
    renderComposer({});

    const submit = screen.getByRole("button", { name: "Generate" });
    expect(submit).toBeDisabled();

    await user.type(screen.getByLabelText("What should it make?"), "a poster");
    expect(submit).toBeEnabled();
  });

  it("turns a 402 into the top-up prompt, with the shortfall", async () => {
    mocks.create.mockRejectedValue(
      new ApiError("insufficient credits", 402, "Payment Required"),
    );

    renderComposer({});
    await submitPrompt();

    expect(await screen.findByText("Not enough credits")).toBeInTheDocument();
    expect(
      screen.getByText("This skill costs 760 credits and you have 100."),
    ).toBeInTheDocument();
  });

  it("turns a 429 into the limits prompt", async () => {
    mocks.create.mockRejectedValue(
      new ApiError("too many requests", 429, "Too Many Requests"),
    );

    renderComposer({});
    await submitPrompt();

    expect(await screen.findByText("Limit reached")).toBeInTheDocument();
    expect(screen.queryByText("Not enough credits")).not.toBeInTheDocument();
  });

  it("keeps the prompt after a failed submit so it can be retried", async () => {
    mocks.create.mockRejectedValue(new Error("network down"));

    renderComposer({});
    await submitPrompt();

    expect(
      await screen.findByText("Could not start the generation. Try again."),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("What should it make?")).toHaveValue(
      "a launch poster",
    );
  });

  it("shows the finished assets and the app's library link", async () => {
    mocks.create.mockResolvedValue(detail());
    mocks.detail.mockReturnValue({
      data: detail({
        status: "completed",
        assets: [
          {
            id: "asset-1",
            generationId: "gen-1",
            kind: "image",
            mediaUrl: "https://cdn.test/poster.png",
            format: "png",
            createdAt: "2026-09-23T00:00:00Z",
          },
        ],
      }),
      isPending: false,
    });

    renderComposer({ worksHref: "/acme/works" });
    await submitPrompt();

    expect(await screen.findByText("Result")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open library" })).toHaveAttribute(
      "href",
      "/acme/works",
    );
  });

  it("omits the library link when the host app supplies no route", async () => {
    mocks.create.mockResolvedValue(detail());
    mocks.detail.mockReturnValue({
      data: detail({ status: "completed" }),
      isPending: false,
    });

    renderComposer({});
    await submitPrompt();

    expect(await screen.findByText("Result")).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "Open library" }),
    ).not.toBeInTheDocument();
  });

  it("spends the submit button once a generation is running", async () => {
    // A second POST reserves the skill's credits again, and the drawer can only
    // follow one generation — so the button is not a retry once the server has
    // accepted the first call.
    mocks.create.mockResolvedValue(detail());
    mocks.detail.mockReturnValue({
      data: detail({ status: "running" }),
      isPending: false,
    });

    renderComposer({});
    await submitPrompt();

    const submit = await screen.findByRole("button", { name: "Generate" });
    expect(submit).toBeDisabled();
    expect(mocks.create).toHaveBeenCalledTimes(1);
  });

  it("tells the user not to retry when the create response could not be read", async () => {
    // A 2xx whose body will not parse means the generation may well exist and
    // its credits are already reserved, so the generic "try again" copy would
    // walk the user into a second charge.
    mocks.create.mockResolvedValue(null);

    renderComposer({});
    await submitPrompt();

    expect(
      await screen.findByText(
        "The generation may already have started, but its response could not be read. Check My works before starting another.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Could not start the generation. Try again."),
    ).not.toBeInTheDocument();
  });

  it("keeps reporting the run when the progress read fails, and can retry it", async () => {
    // The poll gives up on a failed read, so this screen is the only way back —
    // and rendering nothing here is what let the drawer look like a submit that
    // never happened.
    const user = userEvent.setup();
    mocks.create.mockResolvedValue(detail());
    mocks.detail.mockReturnValue({
      data: undefined,
      isPending: false,
      isError: true,
      refetch: mocks.detailRefetch,
    });

    renderComposer({});
    await submitPrompt();

    expect(
      await screen.findByText("Could not read this generation's progress."),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(mocks.detailRefetch).toHaveBeenCalled();
  });

  it("does not state a balance it could not read", async () => {
    // Quoting "you have 0" off an unloaded wallet sends the user to top up
    // against a number the client invented.
    mocks.create.mockRejectedValue(
      new ApiError("insufficient credits", 402, "Payment Required"),
    );
    mocks.balance.mockReturnValue({ data: undefined, isPending: true });

    renderComposer({});
    await submitPrompt();

    expect(await screen.findByText("Not enough credits")).toBeInTheDocument();
    expect(
      screen.getByText("This skill costs 760 credits."),
    ).toBeInTheDocument();
  });
});

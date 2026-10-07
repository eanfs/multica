import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "@multica/core/api";
import type {
  AuroraAsset,
  AuroraGenerationDetail,
  AuroraSkill,
} from "@multica/core/aurora";
import { NavigationProvider } from "../navigation";
import { renderWithI18n } from "../test/i18n";
import { stubNavigationAdapter } from "../test/navigation";

const mocks = vi.hoisted(() => ({ detail: vi.fn(), skills: vi.fn() }));

// The copy path is a UI concern here; jsdom has no clipboard and no
// execCommand, so the real util would report failure on every run.
vi.mock("@multica/ui/lib/clipboard", () => ({
  copyText: vi.fn().mockResolvedValue(true),
}));

vi.mock("@multica/core/aurora", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/aurora")>();
  return {
    ...actual,
    useAuroraGenerationDetail: (id: string) => mocks.detail(id),
    useAuroraSkills: () => mocks.skills(),
  };
});

import { copyText } from "@multica/ui/lib/clipboard";
import { GenerationDetail } from "./generation-detail";

const MICRO = 1_000_000;

const POSTER: AuroraSkill = {
  id: "poster",
  name: "海报制作",
  nameEn: "Poster",
  category: "image",
  credits: 76,
  input: ["text"],
  output: ["image"],
  featured: false,
  available: true,
  attachments: [],
};

function asset(overrides: Partial<AuroraAsset> = {}): AuroraAsset {
  return {
    id: "asset-1",
    generationId: "gen-1",
    kind: "image",
    mediaUrl: "https://cdn.test/poster.png",
    format: "png",
    createdAt: "2026-09-23T00:00:00Z",
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
    status: "completed",
    creditsReserved: 76 * MICRO,
    creditsCharged: 76 * MICRO,
    error: null,
    createdAt: "2026-09-23T00:00:00Z",
    taskId: "",
    assets: [],
    ...overrides,
  };
}

/** A resolved read, shaped the way the parse helpers hand it to the query. */
function read<T>(value: T, degraded = false) {
  return {
    data: { value, degraded },
    isPending: false,
    isError: false,
    refetch: vi.fn(),
  };
}

function pending() {
  return {
    data: undefined,
    isPending: true,
    isError: false,
    refetch: vi.fn(),
  };
}

function failed(error: unknown) {
  return {
    data: undefined,
    isPending: false,
    isError: true,
    error,
    refetch: vi.fn(),
  };
}

function renderDetail(
  props: Partial<Parameters<typeof GenerationDetail>[0]> = {},
) {
  return renderWithI18n(
    <NavigationProvider value={stubNavigationAdapter()}>
      <GenerationDetail generationId="gen-1" {...props} />
    </NavigationProvider>,
  );
}

describe("GenerationDetail", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.detail.mockReturnValue(read(detail()));
    mocks.skills.mockReturnValue(read([POSTER]));
  });

  it("shows the process fields for a completed generation", () => {
    mocks.detail.mockReturnValue(
      read(
        detail({
          assets: [
            asset({ createdAt: "2026-09-23T00:02:00Z" }),
            asset({
              id: "asset-2",
              kind: "video",
              format: "mp4",
              mediaUrl: null,
              createdAt: "2026-09-23T00:01:30Z",
            }),
          ],
        }),
      ),
    );

    const { container } = renderDetail();

    expect(screen.getByText("Poster")).toBeInTheDocument();
    expect(screen.getByText("a launch poster")).toBeInTheDocument();
    expect(
      screen.getByText("Done", { selector: "[data-slot='badge']" }),
    ).toBeInTheDocument();
    expect(container.querySelector("time")).toHaveAttribute(
      "dateTime",
      "2026-09-23T00:00:00Z",
    );
    // The only end timestamp the payload carries is the newest artifact write.
    expect(screen.getByText("2m")).toBeInTheDocument();
    // Reserved and charged are both shown for a settled generation.
    expect(screen.getAllByText("76 credits")).toHaveLength(2);
    expect(screen.getByText("Charged 76 credits")).toBeInTheDocument();
  });

  it("renders an image artifact as a thumbnail and a non-image as a download row", async () => {
    const user = userEvent.setup();
    mocks.detail.mockReturnValue(
      read(
        detail({
          assets: [
            asset(),
            asset({ id: "asset-2", kind: "video", format: "mp4", mediaUrl: null }),
          ],
        }),
      ),
    );

    const { container } = renderDetail();

    const preview = screen.getByRole("button", { name: "Preview png" });
    const image = preview.querySelector("img");
    expect(image).toHaveAttribute("src", "https://cdn.test/poster.png");
    expect(image).toHaveAttribute("loading", "lazy");

    const download = container.querySelector(
      'a[href="/api/aurora/assets/asset-2/download"]',
    );
    expect(download).not.toBeNull();
    expect(within(download as HTMLElement).getByText("mp4")).toBeInTheDocument();
    expect(
      within(download as HTMLElement).getByText("Download"),
    ).toBeInTheDocument();

    await user.click(preview);
    const dialog = await screen.findByRole("dialog");
    expect(
      within(dialog).getByRole("link", { name: "Open in new tab" }),
    ).toHaveAttribute("href", "https://cdn.test/poster.png");
    expect(
      within(dialog).getByRole("link", { name: "Download" }),
    ).toHaveAttribute("href", "/api/aurora/assets/asset-1/download");
  });

  it("shows the failure reason and the refunded state for a failed generation", () => {
    mocks.detail.mockReturnValue(
      read(
        detail({
          status: "failed",
          creditsCharged: 0,
          error: "provider rejected the prompt",
        }),
      ),
    );

    renderDetail();

    expect(
      screen.getByText("Failed", { selector: "[data-slot='badge']" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("provider rejected the prompt"),
    ).toBeInTheDocument();
    // The charged field reads as refunded, and only the reservation is priced.
    expect(screen.getByText("Refunded")).toBeInTheDocument();
    expect(screen.getAllByText("76 credits")).toHaveLength(1);
    expect(
      screen.getByText("No credits were charged; the reservation was refunded."),
    ).toBeInTheDocument();
  });

  it("shows a loading skeleton before the generation resolves", () => {
    mocks.detail.mockReturnValue(pending());

    const { container } = renderDetail();

    expect(
      container.querySelectorAll("[data-slot='skeleton']").length,
    ).toBeGreaterThan(0);
    expect(screen.queryByText("Process")).not.toBeInTheDocument();
  });

  it("reports a generation that does not exist as not found", () => {
    mocks.detail.mockReturnValue(
      failed(new ApiError("generation not found", 404, "Not Found")),
    );

    renderDetail();

    expect(screen.getByText("Generation not found")).toBeInTheDocument();
    expect(
      screen.queryByText("Could not load this generation"),
    ).not.toBeInTheDocument();
  });

  it("reports an unreadable generation as a load failure and retries it", async () => {
    const user = userEvent.setup();
    const refetch = vi.fn();
    mocks.detail.mockReturnValue({
      data: undefined,
      isPending: false,
      isError: true,
      error: new ApiError("boom", 500, "Server Error"),
      refetch,
    });

    renderDetail();

    expect(
      screen.getByText("Could not load this generation"),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(refetch).toHaveBeenCalled();
  });

  it("offers the way back to the works list", () => {
    renderDetail({ backHref: "/acme/works" });

    expect(
      screen.getByRole("link", { name: "Back to My works" }),
    ).toHaveAttribute("href", "/acme/works");
  });

  it("shows a task id in a copyable form when the app has no task route", async () => {
    const user = userEvent.setup();
    mocks.detail.mockReturnValue(read(detail({ taskId: "task-1" })));

    renderDetail();

    expect(screen.getByText("task-1")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Copy task ID" }));
    expect(copyText).toHaveBeenCalledWith("task-1");
    expect(
      await screen.findByRole("button", { name: "Copied" }),
    ).toBeInTheDocument();
  });

  it("links the task when the app provides a task route", () => {
    mocks.detail.mockReturnValue(read(detail({ taskId: "task-1" })));

    renderDetail({ taskHref: (id) => "/acme/tasks/" + id });

    expect(screen.getByRole("link", { name: /task-1/ })).toHaveAttribute(
      "href",
      "/acme/tasks/task-1",
    );
  });
});

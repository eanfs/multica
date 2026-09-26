import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type {
  AuroraAsset,
  AuroraGeneration,
  AuroraSkill,
} from "@multica/core/aurora";
import { renderWithI18n } from "../test/i18n";

// `auroraAssetDownloadPath` is deliberately left real: the href a row points at
// is part of the contract with the download route, not of the query layer being
// replaced here.

const mocks = vi.hoisted(() => ({
  generations: vi.fn(),
  assets: vi.fn(),
  skills: vi.fn(),
  remove: vi.fn(),
}));

vi.mock("@multica/core/aurora", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/aurora")>();
  return {
    ...actual,
    useAuroraGenerations: () => mocks.generations(),
    useAuroraAssets: () => mocks.assets(),
    useAuroraSkills: () => mocks.skills(),
    useDeleteAuroraAsset: () => ({
      mutateAsync: mocks.remove,
      isPending: false,
    }),
  };
});

import { WorksList } from "./works-list";

const MICRO = 1_000_000;

function generation(
  overrides: Partial<AuroraGeneration> = {},
): AuroraGeneration {
  return {
    id: "gen-1",
    skillId: "poster",
    prompt: "a launch poster",
    status: "completed",
    creditsReserved: 760 * MICRO,
    ...overrides,
  };
}

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
  attachments: [],
};

function renderWorks() {
  return renderWithI18n(<WorksList />);
}

/**
 * A resolved read, shaped the way `parseAurora*` hands it to the query: the
 * payload plus whether the body behind it was readable.
 */
function read<T>(value: T, degraded = false) {
  return { data: { value, degraded }, isPending: false };
}

describe("WorksList", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.generations.mockReturnValue(read([generation()]));
    mocks.assets.mockReturnValue(read([asset()]));
    mocks.skills.mockReturnValue(read([POSTER]));
  });

  it("lists a generation with the skill that produced it", () => {
    renderWorks();

    expect(screen.getByText("a launch poster")).toBeInTheDocument();
    expect(screen.getByText("Poster")).toBeInTheDocument();
    expect(screen.getByText("Done")).toBeInTheDocument();
    expect(screen.getByText("760 credits")).toBeInTheDocument();
  });

  it("does not price a generation that was refunded", () => {
    // A failed generation is refunded in full (aurora.go's completion path
    // writes creditsCharged 0), so the amount it reserved is not a cost —
    // printing it under "credits" claims a spend that never happened.
    mocks.generations.mockReturnValue(read([generation({ status: "failed" })]));

    renderWorks();

    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(screen.queryByText("760 credits")).not.toBeInTheDocument();
  });

  it("names the skill as unknown rather than blank when the catalog has dropped it", () => {
    // A generation outlives the catalog entry that made it, so the row has to
    // say something the reader can act on instead of rendering nothing.
    mocks.generations.mockReturnValue(
      read([generation({ skillId: "retired-skill" })]),
    );

    renderWorks();

    expect(screen.getByText("Unknown skill")).toBeInTheDocument();
  });

  it("links every asset to its download route", () => {
    mocks.assets.mockReturnValue(
      read([asset(), asset({ id: "asset-2", kind: "video", format: "mp4" })]),
    );

    renderWorks();

    const links = screen.getAllByRole("link", { name: "Download" });
    expect(links).toHaveLength(2);
    expect(links[0]).toHaveAttribute(
      "href",
      "/api/aurora/assets/asset-1/download",
    );
    expect(links[1]).toHaveAttribute(
      "href",
      "/api/aurora/assets/asset-2/download",
    );
  });

  it("does not repeat an asset's kind when it has no format to show above it", () => {
    mocks.assets.mockReturnValue(read([asset({ format: null })]));

    renderWorks();

    expect(screen.getAllByText("image")).toHaveLength(1);
  });

  it("deletes an asset only once the confirmation is accepted", async () => {
    const user = userEvent.setup();
    mocks.remove.mockResolvedValue(undefined);

    renderWorks();
    await user.click(screen.getByRole("button", { name: "Delete" }));

    expect(await screen.findByText("Delete this file?")).toBeInTheDocument();
    expect(mocks.remove).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "Delete file" }));

    expect(mocks.remove).toHaveBeenCalledWith("asset-1");
  });

  it("keeps the row and reports the failure when a delete fails", async () => {
    const user = userEvent.setup();
    mocks.remove.mockRejectedValue(new Error("nope"));

    renderWorks();
    await user.click(screen.getByRole("button", { name: "Delete" }));
    await user.click(
      await screen.findByRole("button", { name: "Delete file" }),
    );

    // The server drops the stored object as well as the row, so a failed delete
    // must not leave the list claiming a file the user can no longer fetch is
    // gone — and must not imply it is still there either.
    expect(
      await screen.findByText("Could not delete the file. Try again."),
    ).toBeInTheDocument();
    expect(screen.getByText("png")).toBeInTheDocument();
  });

  it("closes the confirmation when a delete fails, so the report is not stuck behind it", async () => {
    // `AlertDialogAction` is a plain Button, not a close primitive: leaving the
    // dialog open on failure covered the report with a modal backdrop that also
    // marks everything outside it `aria-hidden`. Sighted users saw a dialog
    // that did nothing; screen-reader users heard nothing at all.
    const user = userEvent.setup();
    mocks.remove.mockRejectedValue(new Error("nope"));

    renderWorks();
    await user.click(screen.getByRole("button", { name: "Delete" }));
    await user.click(
      await screen.findByRole("button", { name: "Delete file" }),
    );
    await screen.findByText("Could not delete the file. Try again.");

    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
  });

  it("stays silent about unknown skills when the catalog itself could not be read", async () => {
    // A failed catalog request used to stamp "Unknown skill" on every row — a
    // screenful of claims the client had no basis for. It is only unknown when
    // the catalog answered and did not list it.
    mocks.skills.mockReturnValue({
      data: undefined,
      isPending: false,
      isError: true,
    });

    renderWorks();

    expect(screen.queryByText("Unknown skill")).not.toBeInTheDocument();
    expect(screen.getByText("a launch poster")).toBeInTheDocument();
  });

  it("stays silent about unknown skills when the catalog body could not be read", () => {
    // A degraded catalog parses to an empty list, so it is not "the catalog
    // dropped this entry" either — same silence as a failed read.
    mocks.skills.mockReturnValue(read([], true));

    renderWorks();

    expect(screen.queryByText("Unknown skill")).not.toBeInTheDocument();
    expect(screen.getByText("a launch poster")).toBeInTheDocument();
  });

  it("offers the empty copy before anything has been generated", () => {
    mocks.generations.mockReturnValue(read([]));
    mocks.assets.mockReturnValue(read([]));

    renderWorks();

    expect(screen.getByText("No generations yet")).toBeInTheDocument();
    expect(screen.getByText("No content yet")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
  });

  it("reports a list it could not read as a failed load, not as an empty library", () => {
    // GH #55: a malformed 200 parses to [] and resolves the query, so the
    // screen used to claim the workspace had generated nothing.
    mocks.generations.mockReturnValue(read([], true));
    mocks.assets.mockReturnValue(read([], true));

    renderWorks();

    expect(screen.getByText("Could not load your works")).toBeInTheDocument();
    expect(screen.queryByText("No generations yet")).not.toBeInTheDocument();
    expect(screen.queryByText("No content yet")).not.toBeInTheDocument();
  });

  it("reports the unreadable half even when the other list resolved", () => {
    // One read is enough: the library is one screen, and a screen that cannot
    // be trusted about its assets is not a screen that can be trusted about
    // anything on it. Same rule a failed read already followed.
    mocks.assets.mockReturnValue(read([], true));

    renderWorks();

    expect(screen.getByText("Could not load your works")).toBeInTheDocument();
    expect(screen.queryByText("a launch poster")).not.toBeInTheDocument();
  });
});

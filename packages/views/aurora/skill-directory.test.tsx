import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AuroraSkill } from "@multica/core/aurora";
import { renderWithI18n } from "../test/i18n";
import { stubNavigationAdapter } from "../test/navigation";
import { NavigationProvider } from "../navigation";

const mocks = vi.hoisted(() => ({
  skills: vi.fn(),
  create: vi.fn(),
  detail: vi.fn(),
  balance: vi.fn(),
}));

vi.mock("@multica/core/aurora", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/aurora")>();
  return {
    ...actual,
    useAuroraSkills: () => mocks.skills(),
    // The directory owns the drawer, so the composer's hooks resolve here too.
    useCreateAuroraGeneration: () => ({
      mutateAsync: mocks.create,
      isPending: false,
    }),
    useAuroraGenerationDetail: (id: string) => mocks.detail(id),
    useAuroraBalance: () => mocks.balance(),
  };
});

import { SkillDirectory } from "./skill-directory";

/**
 * The shipped catalog (`server/internal/aurora/catalog.go`), id by id: 16
 * entries, of which avatar-video, ppt and excel are listed but not runnable.
 * The counts in these tests are the catalog's own contract, so the fixture
 * mirrors it rather than shrinking to a convenient sample.
 */
const CATALOG: AuroraSkill[] = [
  ["poster", "海报制作", "Poster", "image", 760],
  ["xhs-image", "小红书图片", "Xiaohongshu Image", "image", 620],
  ["product-image", "商品图制作", "Product Image", "image", 860],
  ["text-image", "文字生成图片", "Text to Image", "image", 680],
  ["image-edit", "图片修改", "Image Edit", "image", 520],
  ["id-photo", "证件照制作", "ID Photo", "image", 360],
  ["image-video", "图片生成视频", "Image to Video", "video", 1880],
  ["text-video", "文字生成视频", "Text to Video", "video", 1680],
  ["video-captions", "视频剪辑与字幕", "Video Captions", "video", 980],
  ["xhs-copy", "小红书文案", "Xiaohongshu Copy", "content", 260],
  ["resume", "简历制作", "Resume", "office", 420],
  ["document-summary", "文件总结", "Document Summary", "office", 380],
  ["transcription", "录音转文字", "Transcription", "office", 300],
].map(([id, name, nameEn, category, credits]) => ({
  id: id as string,
  name: name as string,
  nameEn: nameEn as string,
  category: category as string,
  credits: credits as number,
  input: ["text"],
  output: ["image"],
  featured: false,
  available: true,
}));

const UNAVAILABLE: AuroraSkill[] = [
  {
    id: "avatar-video",
    name: "数字人口播",
    nameEn: "Avatar Video",
    category: "video",
    credits: 1480,
    input: ["text"],
    output: ["video"],
    featured: false,
    available: false,
  },
  {
    id: "ppt",
    name: "PPT 制作",
    nameEn: "PPT",
    category: "office",
    credits: 820,
    input: ["text"],
    output: ["pptx"],
    featured: false,
    available: false,
  },
  {
    id: "excel",
    name: "Excel 数据分析",
    nameEn: "Excel Analysis",
    category: "office",
    credits: 460,
    input: ["text"],
    output: ["xlsx"],
    featured: false,
    available: false,
  },
];

const ALL_SKILLS = [...CATALOG.slice(0, 9), UNAVAILABLE[0], ...CATALOG.slice(9), ...UNAVAILABLE.slice(1)];

function renderDirectory() {
  return renderWithI18n(
    <NavigationProvider value={stubNavigationAdapter()}>
      <SkillDirectory />
    </NavigationProvider>,
  );
}

/** The skill cards, as opposed to the category tabs and the search box. */
function cards() {
  return within(screen.getByRole("list")).getAllByRole("button");
}

describe("SkillDirectory", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.skills.mockReturnValue({ data: ALL_SKILLS, isPending: false });
    mocks.detail.mockReturnValue({ data: undefined, isPending: true });
    mocks.balance.mockReturnValue({
      data: { availableMicro: 0 },
      isPending: false,
    });
  });

  it("lists the whole catalog and flags the skills that have not launched", () => {
    renderDirectory();

    expect(cards()).toHaveLength(16);
    expect(screen.getAllByText("Coming soon")).toHaveLength(3);
  });

  it("narrows the grid to one category", async () => {
    const user = userEvent.setup();
    renderDirectory();

    await user.click(screen.getByRole("tab", { name: "Video" }));

    // Four video skills, one of which is the unavailable avatar-video.
    expect(cards()).toHaveLength(4);
    expect(screen.getAllByText("Coming soon")).toHaveLength(1);
  });

  it("narrows the grid to a search across both catalog names", async () => {
    const user = userEvent.setup();
    renderDirectory();

    await user.type(screen.getByPlaceholderText("Search skills"), "海报");

    expect(cards()).toHaveLength(1);
    expect(screen.getByRole("button", { name: /Poster/ })).toBeInTheDocument();
  });

  it("says so when a search matches nothing", async () => {
    const user = userEvent.setup();
    renderDirectory();

    await user.type(screen.getByPlaceholderText("Search skills"), "zzzz");

    expect(screen.getByText("No matching skills")).toBeInTheDocument();
    expect(screen.queryByRole("list")).not.toBeInTheDocument();
  });

  it("offers the composer for the skill that was chosen", async () => {
    const user = userEvent.setup();
    renderDirectory();

    await user.click(screen.getByRole("button", { name: /Poster/ }));

    expect(
      await screen.findByRole("button", { name: "Generate" }),
    ).toBeInTheDocument();
    expect(screen.getByText("760 credits per generation")).toBeInTheDocument();
  });

  it("offers the composer for a skill that has not launched, so the reason is reachable", async () => {
    const user = userEvent.setup();
    renderDirectory();

    await user.click(screen.getByRole("button", { name: /PPT/ }));

    expect(await screen.findByText("Not available yet")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Generate" })).toBeDisabled();
  });

  it("offers a retry instead of an empty grid when the catalog cannot be read", async () => {
    const user = userEvent.setup();
    const refetch = vi.fn();
    mocks.skills.mockReturnValue({
      data: undefined,
      isPending: false,
      isError: true,
      refetch,
    });
    renderDirectory();

    expect(
      screen.getByText("Could not load the skill directory"),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(refetch).toHaveBeenCalled();
  });
});

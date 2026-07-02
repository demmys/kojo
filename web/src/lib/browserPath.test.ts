import { describe, expect, it } from "vitest";
import {
  basename,
  buildBreadcrumbs,
  joinBrowserPath,
  normalizePath,
  parentBrowserPath,
  sanitizeRelativePath,
  samePath,
} from "./browserPath";

describe("sanitizeRelativePath", () => {
  it("strips traversal segments and normalizes separators", () => {
    expect(sanitizeRelativePath("../../etc/passwd")).toBe("etc/passwd");
    expect(sanitizeRelativePath("a/./b/../c")).toBe("a/b/c");
    expect(sanitizeRelativePath("foo\\bar\\baz")).toBe("foo/bar/baz");
    expect(sanitizeRelativePath("//a///b//")).toBe("a/b");
    expect(sanitizeRelativePath("..")).toBe("");
    expect(sanitizeRelativePath("")).toBe("");
  });
});

describe("normalizePath", () => {
  it("sanitizes in relative mode, passes through in absolute mode", () => {
    expect(normalizePath("../a/b", "relative")).toBe("a/b");
    expect(normalizePath("/../a/b", "absolute")).toBe("/../a/b");
    expect(normalizePath("C:\\Users\\x", "absolute")).toBe("C:\\Users\\x");
  });
});

describe("joinBrowserPath", () => {
  it("joins relative paths safely", () => {
    expect(joinBrowserPath("a/b", "c", "relative")).toBe("a/b/c");
    expect(joinBrowserPath("a", "../evil", "relative")).toBe("a/evil");
    expect(joinBrowserPath("", "root", "relative")).toBe("root");
  });
  it("joins absolute paths preserving separator style", () => {
    expect(joinBrowserPath("/home/user", "file", "absolute")).toBe("/home/user/file");
    expect(joinBrowserPath("/home/user/", "file", "absolute")).toBe("/home/user/file");
    expect(joinBrowserPath("C:\\Users", "x", "absolute")).toBe("C:\\Users\\x");
    expect(joinBrowserPath("", "name", "absolute")).toBe("name");
  });
});

describe("parentBrowserPath", () => {
  it("relative mode drops last segment", () => {
    expect(parentBrowserPath("a/b/c", "relative")).toBe("a/b");
    expect(parentBrowserPath("a", "relative")).toBe("");
    expect(parentBrowserPath("", "relative")).toBe("");
  });
  it("absolute mode handles roots and drives", () => {
    expect(parentBrowserPath("/home/user", "absolute")).toBe("/home");
    expect(parentBrowserPath("/home", "absolute")).toBe("/");
    expect(parentBrowserPath("/", "absolute")).toBe("/");
    expect(parentBrowserPath("C:\\Users\\x", "absolute")).toBe("C:\\Users");
    expect(parentBrowserPath("C:\\", "absolute")).toBe("C:\\");
  });
});

describe("basename", () => {
  it("extracts the final segment", () => {
    expect(basename("/home/user/file.txt")).toBe("file.txt");
    expect(basename("a/b/")).toBe("b");
    expect(basename("C:\\Users\\x")).toBe("x");
    expect(basename("solo")).toBe("solo");
  });
});

describe("samePath", () => {
  it("compares case-insensitively for drive paths, case-sensitively otherwise", () => {
    expect(samePath("/a/b", "/a/b/")).toBe(true);
    expect(samePath("/a/b", "/a/B")).toBe(false);
    expect(samePath("C:\\Users", "c:/users")).toBe(true);
  });
});

describe("buildBreadcrumbs", () => {
  it("relative mode builds cumulative crumbs from root label", () => {
    const crumbs = buildBreadcrumbs("a/b/c", "relative", "Home");
    expect(crumbs).toEqual([
      { label: "Home", path: "", isRoot: true },
      { label: "a", path: "a" },
      { label: "b", path: "a/b" },
      { label: "c", path: "a/b/c" },
    ]);
  });
  it("relative mode ignores traversal in the input path", () => {
    const crumbs = buildBreadcrumbs("../../x", "relative", "Home");
    expect(crumbs.map((c) => c.label)).toEqual(["Home", "x"]);
  });
  it("absolute mode with rootPath makes crumbs relative to the root", () => {
    const crumbs = buildBreadcrumbs("/srv/data/sub", "absolute", "root", "/srv/data");
    expect(crumbs).toEqual([
      { label: "root", path: "/srv/data", isRoot: true },
      { label: "sub", path: "/srv/data/sub" },
    ]);
  });
  it("absolute mode without rootPath walks from filesystem root", () => {
    const crumbs = buildBreadcrumbs("/a/b", "absolute", "/");
    expect(crumbs).toEqual([
      { label: "/", path: "/", isRoot: true },
      { label: "a", path: "/a" },
      { label: "b", path: "/a/b" },
    ]);
  });
});

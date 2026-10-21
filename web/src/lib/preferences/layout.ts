// Sidebar Variant
export const SIDEBAR_VARIANT_OPTIONS = [
  { label: "サイドバー", value: "sidebar" },
  { label: "内側", value: "inset" },
  { label: "フローティング", value: "floating" },
] as const;
export const SIDEBAR_VARIANT_VALUES = SIDEBAR_VARIANT_OPTIONS.map((v) => v.value);
export type SidebarVariant = (typeof SIDEBAR_VARIANT_VALUES)[number];

// Sidebar Collapsible
export const SIDEBAR_COLLAPSIBLE_OPTIONS = [
  { label: "アイコン", value: "icon" },
  { label: "画面外に収納", value: "offcanvas" },
] as const;
export const SIDEBAR_COLLAPSIBLE_VALUES = SIDEBAR_COLLAPSIBLE_OPTIONS.map((v) => v.value);
export type SidebarCollapsible = (typeof SIDEBAR_COLLAPSIBLE_VALUES)[number];

// Content Layout
export const CONTENT_LAYOUT_OPTIONS = [
  { label: "中央寄せ", value: "centered" },
  { label: "全幅", value: "full-width" },
] as const;
export const CONTENT_LAYOUT_VALUES = CONTENT_LAYOUT_OPTIONS.map((v) => v.value);
export type ContentLayout = (typeof CONTENT_LAYOUT_VALUES)[number];

// Navbar Style
export const NAVBAR_STYLE_OPTIONS = [
  { label: "固定", value: "sticky" },
  { label: "スクロール", value: "scroll" },
] as const;
export const NAVBAR_STYLE_VALUES = NAVBAR_STYLE_OPTIONS.map((v) => v.value);
export type NavbarStyle = (typeof NAVBAR_STYLE_VALUES)[number];

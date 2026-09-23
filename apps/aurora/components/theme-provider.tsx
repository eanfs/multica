"use client"

// Re-export the shared ThemeProvider from @multica/ui. Aurora needs no
// React 19 <script> warning shim, which is the only thing apps/web's copy of
// this file adds on top.
export { ThemeProvider } from "@multica/ui/components/common/theme-provider"

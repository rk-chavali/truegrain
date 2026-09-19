import "@testing-library/jest-dom/vitest";
import { afterEach, vi } from "vitest";
import { cleanup } from "@testing-library/react";

/*
  Test environment.

  jsdom does not implement matchMedia or ResizeObserver, and Mantine
  reads both: the colour scheme comes from a media query and several
  components measure themselves. Without these stubs every test
  rendering a Mantine component throws before it asserts anything.
*/

Object.defineProperty(window, "matchMedia", {
  writable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: vi.fn(),
    removeListener: vi.fn(),
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
  }),
});

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
window.ResizeObserver = ResizeObserverStub;

// jsdom has no layout, so scrollIntoView is missing and Mantine's
// combobox calls it when the highlighted option changes.
Element.prototype.scrollIntoView = vi.fn();

afterEach(cleanup);

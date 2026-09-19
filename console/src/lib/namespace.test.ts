import { expect, test } from "vitest";
import { ALL, inScope, splitName } from "./namespace";

/*
  Namespace scoping.

  Names arrive qualified, and how far the first dot reaches decides
  whether scoping works at all. A metric is `retail.order_revenue` and
  a dimension is `retail.customers.region`: same namespace, different
  depth, so anything that splits on every dot gets one of them wrong.
*/

test("only the first segment is the namespace", () => {
  expect(splitName("retail.order_revenue")).toEqual({
    namespace: "retail",
    rest: "order_revenue",
  });
  // A dimension carries its table, and the table is not a namespace.
  expect(splitName("retail.customers.region")).toEqual({
    namespace: "retail",
    rest: "customers.region",
  });
});

test("an unqualified name has no namespace rather than becoming one", () => {
  expect(splitName("order_revenue")).toEqual({ namespace: "", rest: "order_revenue" });
});

test("scoping keeps both depths of name in the same namespace", () => {
  expect(inScope("retail.order_revenue", "retail")).toBe(true);
  expect(inScope("retail.customers.region", "retail")).toBe(true);
  expect(inScope("finance.order_revenue", "retail")).toBe(false);
});

test("a prefix that merely starts the same is a different namespace", () => {
  /*
    The bug this prevents: matching with startsWith, which puts
    retail_eu's metrics inside retail and quietly mixes two models.
  */
  expect(inScope("retail_eu.order_revenue", "retail")).toBe(false);
});

test("no scope shows everything", () => {
  expect(inScope("retail.order_revenue", ALL)).toBe(true);
  expect(inScope("finance.spend", ALL)).toBe(true);
});

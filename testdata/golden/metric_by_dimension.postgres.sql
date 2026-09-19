SELECT
  "customers"."region" AS "region",
  SUM("orders"."order_total") AS "order_revenue"
FROM "main"."orders" AS "orders"
  LEFT JOIN "main"."customers" AS "customers" ON "orders"."customer_id" = "customers"."customer_id"
GROUP BY 1
ORDER BY "order_revenue" DESC
LIMIT 10

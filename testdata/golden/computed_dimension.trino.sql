SELECT
  LOWER(SUBSTR("customers"."email", POSITION('@' IN "customers"."email") + 1)) AS "email_domain",
  SUM("orders"."order_total") AS "order_revenue"
FROM "main"."orders" AS "orders"
  LEFT JOIN "main"."customers" AS "customers" ON "orders"."customer_id" = "customers"."customer_id"
GROUP BY 1
LIMIT 1000

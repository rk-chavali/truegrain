SELECT
  date_trunc('month', "orders"."order_date") AS "order_date",
  SUM("orders"."order_total") AS "order_revenue",
  COUNT(DISTINCT "orders"."order_id") AS "order_count"
FROM "main"."orders" AS "orders"
GROUP BY 1
LIMIT 1000

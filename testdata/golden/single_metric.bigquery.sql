SELECT
  SUM(`orders`.`order_total`) AS `order_revenue`
FROM `main.orders` AS `orders`
LIMIT 1000

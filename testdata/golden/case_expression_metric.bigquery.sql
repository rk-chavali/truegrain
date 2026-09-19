SELECT
  `orders`.`status` AS `status`,
  SUM(CASE WHEN `orders`.`status` = 'shipped' THEN `orders`.`order_total` ELSE 0 END) AS `shipped_revenue`
FROM `main.orders` AS `orders`
GROUP BY 1
LIMIT 1000

SELECT
  `customers`.`region` AS `region`,
  SUM(`orders`.`order_total`) / COUNT(DISTINCT `orders`.`order_id`) AS `average_order_value`
FROM `main`.`orders` AS `orders`
  LEFT JOIN `main`.`customers` AS `customers` ON `orders`.`customer_id` = `customers`.`customer_id`
GROUP BY 1
LIMIT 1000

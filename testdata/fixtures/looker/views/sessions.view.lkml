# A derived table: SQL rather than a physical relation.
view: sessions {
  derived_table: {
    sql: SELECT user_id, count(*) AS n FROM events GROUP BY 1 ;;
  }
  dimension: user_id {
    type: number
  }
}

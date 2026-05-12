class Animal
  def initialize(name)
    @name = name
  end

  def speak
    "..."
  end
end

def create_animal(name)
  Animal.new(name)
end
